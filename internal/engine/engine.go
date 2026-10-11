// Package engine télécharge un fichier par morceaux parallèles, avec pause (Ctrl+C) et reprise,
// sur le modèle d'Internet Download Manager : chaque morceau reprend à l'endroit exact où il s'était arrêté.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"grab/internal/store"
)

const (
	userAgent       = "Mozilla/5.0 (compatible; grab/1.0)"
	defaultSegments = 8
	maxSegments     = 32
	flushEvery      = 250 * time.Millisecond
	maxFreshCycles  = 3
)

// ErrStopped signale un arrêt volontaire (Ctrl+C), pas une panne.
var ErrStopped = errors.New("stopped")

// Engine lance et supervise les téléchargements ; l'écran s'abonne via Reporter.
type Engine struct {
	Client   *http.Client
	Store    *store.Store
	Reporter func(store.Progress)

	base   context.Context
	dmu    sync.Mutex // protège les écritures de progression lues par l'enregistreur périodique
	mu     sync.Mutex
	stops  map[string]context.CancelFunc
	jobsWG sync.WaitGroup
}

// New prépare le moteur ; les téléchargements naissent du contexte base : quand il s'annule
// (Ctrl+C), chaque téléchargement se met en pause au lieu de partir en panne.
func New(base context.Context, db *store.Store, reporter func(store.Progress)) *Engine {
	return &Engine{
		base:     base,
		Client:   &http.Client{},
		Store:    db,
		Reporter: reporter,
		stops:    make(map[string]context.CancelFunc),
	}
}

// Start lance le téléchargement identifié par id ; renvoie faux s'il tourne déjà.
// Le téléchargement s'exécute en tâche de fond ; Wait attend que tout soit fini.
func (e *Engine) Start(id string) bool {
	e.mu.Lock()
	if _, busy := e.stops[id]; busy {
		e.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(e.base)
	e.stops[id] = cancel
	e.jobsWG.Add(1)
	e.mu.Unlock()
	go e.runJob(ctx, id, cancel)
	return true
}

// Stop demande la pause d'un téléchargement en cours.
func (e *Engine) Stop(id string) {
	e.mu.Lock()
	cancel, ok := e.stops[id]
	e.mu.Unlock()
	if ok {
		cancel()
	}
}

// StopAll demande la pause de tous les téléchargements en cours.
func (e *Engine) StopAll() {
	e.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(e.stops))
	for _, cancel := range e.stops {
		cancels = append(cancels, cancel)
	}
	e.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Wait attend la fin de tous les téléchargements lancés.
func (e *Engine) Wait() { e.jobsWG.Wait() }

// Idle dit si aucun téléchargement ne tourne en ce moment.
func (e *Engine) Idle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.stops) == 0
}

func (e *Engine) runJob(parent context.Context, id string, cancel context.CancelFunc) {
	defer e.jobsWG.Done()
	e.mu.Lock()
	delete(e.stops, id)
	e.mu.Unlock()

	d, ok := e.Store.Claim(id)
	if !ok {
		// déjà en cours, déjà fini, ou disparu : rien à lancer
		cancel()
		return
	}
	ctx, jobCancel := context.WithCancel(parent)
	defer jobCancel()

	flushStop := make(chan struct{})
	defer close(flushStop)
	go e.flushLoop(ctx, &d, flushStop)

	d.Status = store.StatusRunning
	d.Error = ""
	e.flush(&d)

	err := e.run(ctx, &d)

	switch {
	case ctx.Err() != nil:
		d.Status = store.StatusPaused
		d.Error = ""
	case err == nil:
		d.Status = store.StatusDone
		now := time.Now()
		d.FinishedAt = &now
		d.Error = ""
	default:
		d.Status = store.StatusFailed
		d.Error = err.Error()
	}
	e.flush(&d)
}

func (e *Engine) flushLoop(ctx context.Context, d *store.Download, stop chan struct{}) {
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.flush(d)
		}
	}
}

// flush repousse l'état courant dans le fichier sur disque et prévient l'écran.
func (e *Engine) flush(d *store.Download) {
	e.dmu.Lock()
	cp := *d
	cp.Chunks = make([]store.Chunk, len(d.Chunks))
	copy(cp.Chunks, d.Chunks)
	cp.UpdatedAt = time.Now()
	report := store.Progress{ID: d.ID, Name: d.Name, Status: d.Status, Done: d.BytesDone(), Total: d.Size}
	e.dmu.Unlock()

	_ = e.Store.Update(&cp)
	if e.Reporter != nil {
		e.Reporter(report)
	}
}

// run porte le cycle complet : sonde, découpe, morceaux en parallèle, finalisation.
func (e *Engine) run(ctx context.Context, d *store.Download) error {
	if err := os.MkdirAll(d.Options.Dir, 0o755); err != nil {
		return fmt.Errorf("dossier de destination inaccessible : %w", err)
	}
	if len(d.Chunks) == 0 {
		if err := e.inspect(ctx, d); err != nil {
			return err
		}
		if d.Size == 0 {
			return e.writeEmptyFile(d)
		}
		if len(d.Chunks) == 0 {
			// pour les flux segmentés, la sonde a déjà découpé : ne pas ré-égratiller par-dessus
			e.dmu.Lock()
			d.Chunks = e.planChunks(d.Size, d.Options.Segments)
			e.dmu.Unlock()
		}
	}
	// le fichier incomplet a peut-être disparu (nettoyage manuel) : dans ce cas, repartir de zéro
	if _, err := os.Stat(d.PartPath()); os.IsNotExist(err) {
		e.dmu.Lock()
		for i := range d.Chunks {
			d.Chunks[i].Written = 0
			d.Chunks[i].Done = false
		}
		e.dmu.Unlock()
	}
	file, err := os.OpenFile(d.PartPath(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("fichier de travail inaccessible : %w", err)
	}
	defer file.Close()

	for cycle := 0; cycle < maxFreshCycles; cycle++ {
		if ctx.Err() != nil {
			return ErrStopped
		}
		// flux segmenté (HLS) : chaque morceau porte son adresse, on recoud les tranches dans l'ordre
		if len(d.Chunks) > 0 && d.Chunks[0].URL != "" {
			if err := e.fetchHLS(ctx, d, file); err != nil {
				return err
			}
			if err := e.finalize(d, file); err != nil {
				return err
			}
			e.maybeRemux(d)
			return nil
		}
		if err := e.fetchAllOnce(ctx, d, file); err != nil {
			switch {
			case errors.Is(err, errIsPlaylist):
				// le fichier demandé est en réalité une playlist : on la suit à la lettre
				if err := e.prepareHLS(ctx, d); err != nil {
					return err
				}
				continue
			case errors.Is(err, errNeedFresh) && d.Ranged:
				// le serveur ignore les plages d'octets : un seul téléchargement, du début à la fin
				e.dmu.Lock()
				d.Ranged = false
				if d.Size > 0 {
					d.Chunks = []store.Chunk{{Start: 0, End: d.Size - 1}}
				} else {
					d.Chunks = []store.Chunk{{Start: 0, End: -1}}
				}
				e.dmu.Unlock()
				_ = file.Truncate(0)
				continue
			default:
				return err
			}
		}
		return e.finalize(d, file)
	}
	return errors.New("le serveur refuse obstinément de servir les morceaux demandés")
}

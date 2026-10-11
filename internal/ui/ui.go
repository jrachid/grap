// Package ui affiche l'état des téléchargements pendant qu'ils tournent, avec barre de progression, vitesse et temps restant.
package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"grab/internal/store"
)

// HumanBytes met une taille en forme : « 12.3 Mo ».
func HumanBytes(b int64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%d o", b)
	}
	div, exp := int64(unit), 0
	n := float64(b) / unit
	for n >= unit && exp < 3 {
		n /= unit
		div *= unit
		exp++
	}
	suffix := [...]string{"Ko", "Mo", "Go", "To"}[exp]
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), suffix)
}

// HumanDuration met une durée en forme : « 1:02:07 ».
func HumanDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	case m > 0:
		return fmt.Sprintf("%d:%02d", m, s)
	}
	return fmt.Sprintf("0:%02d", s)
}

// IsTTY dit si un flux accepte les dessins mis à jour en continu.
func IsTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

const barWidth = 22

func bar(pct float64) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	fill := int(pct / 100 * float64(barWidth))
	return strings.Repeat("█", fill) + strings.Repeat("░", barWidth-fill)
}

// Clip coupe un texte s'il dépasse une largeur donnée.
func Clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func statRank(s string) int {
	switch s {
	case store.StatusRunning:
		return 0
	case store.StatusPaused:
		return 1
	case store.StatusFailed:
		return 2
	case store.StatusQueued:
		return 3
	case store.StatusDone:
		return 4
	}
	return 9
}

// StatLabel renvoie le libellé français d'un état de téléchargement.
func StatLabel(s string) string {
	switch s {
	case store.StatusQueued:
		return "en attente"
	case store.StatusRunning:
		return "en cours"
	case store.StatusPaused:
		return "en pause"
	case store.StatusDone:
		return "terminé"
	case store.StatusFailed:
		return "échec"
	}
	return s
}

// Renderer affiche les téléchargements ; Push reçoit les instantanés du moteur.
type Renderer struct {
	mu        sync.Mutex
	tty       bool
	rows      map[string]*rowState
	ids       []string // ordre d'inscription ; le dessin trie ensuite par état
	lastLines int
	stamp     time.Time
	ch        chan store.Progress
	wake      chan struct{}
}

type rowState struct {
	prog      store.Progress
	startDone int64     // bytes done at the start of the job
	startAt   time.Time // when the job started
	speed     float64
	painted   bool
}

// New prépare l'affichage et détecte si l'écran consent à être redessiné en continu.
func New() *Renderer {
	return &Renderer{
		tty:  IsTTY(os.Stdout),
		rows: make(map[string]*rowState),
		ch:   make(chan store.Progress, 128),
		wake: make(chan struct{}, 1),
	}
}

// Push enregistre un instantané ; jamais bloquant.
func (r *Renderer) Push(p store.Progress) {
	select {
	case r.ch <- p:
	default:
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Serve boucle de dessin jusqu'à l'arrêt du contexte.
func (r *Renderer) Serve(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
		draining := true
		for draining {
			select {
			case p := <-r.ch:
				r.accept(p)
			default:
				draining = false
				r.draw()
			}
		}
	}
}

// accept intègre un instantané du moteur dans les lignes à dessiner.
func (r *Renderer) accept(p store.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rw, seen := r.rows[p.ID]
	if !seen {
		rw = &rowState{prog: p}
		r.rows[p.ID] = rw
		r.ids = append(r.ids, p.ID)
		return
	}
	rw.prog = p
}

// draw peint le bloc entier des téléchargements connus.
func (r *Renderer) draw() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	sorted := make([]string, len(r.ids))
	copy(sorted, r.ids)
	sort.SliceStable(sorted, func(i, j int) bool {
		return statRank(r.rows[sorted[i]].prog.Status) < statRank(r.rows[sorted[j]].prog.Status)
	})

	var lines []string
	for _, id := range sorted {
		rw := r.rows[id]

		// vitesse moyenne depuis le début du téléchargement, stable pour l'ETA
		if rw.prog.Status == store.StatusRunning {
			if rw.startAt.IsZero() {
				rw.startAt = now
				rw.startDone = rw.prog.Done
			} else if dt := now.Sub(rw.startAt).Seconds(); dt >= 0.5 {
				rw.speed = float64(rw.prog.Done-rw.startDone) / dt
			}
		}

		lines = append(lines, r.renderRow(id, rw, now))
	}

	// retire du bloc les téléchargements terminés ou échoués, après un premier affichage
	var keepIDs []string
	for _, id := range r.ids {
		rw := r.rows[id]
		if (rw.prog.Status == store.StatusDone || rw.prog.Status == store.StatusFailed) && rw.painted {
			delete(r.rows, id)
			continue
		}
		keepIDs = append(keepIDs, id)
	}
	r.ids = keepIDs

	if r.tty {
		if r.lastLines > 0 {
			fmt.Fprintf(os.Stdout, "\033[%dA", r.lastLines)
		}
		for i, line := range lines {
			if i < len(lines)-1 {
				fmt.Fprintf(os.Stdout, "\033[K%s\n", line)
			} else {
				fmt.Fprintf(os.Stdout, "\033[K%s", line)
			}
		}
		r.lastLines = len(lines)
		return
	}

	if now.Sub(r.stamp) < 2*time.Second {
		return
	}
	r.stamp = now
	for _, line := range lines {
		fmt.Fprintf(os.Stdout, "%s\n", line)
	}
}

// renderRow compose la ligne affichée pour un téléchargement.
func (r *Renderer) renderRow(id string, rw *rowState, now time.Time) string {
	marker := fmt.Sprintf("#%s", Clip(id, 8))
	name := Clip(filepath.Base(rw.prog.Name), 26)
	base := fmt.Sprintf("%-9s %-26s %-10s", marker, name, StatLabel(rw.prog.Status))

	switch rw.prog.Status {
	case store.StatusRunning:
		done, total := rw.prog.Done, rw.prog.Total
		speed := ""
		if rw.speed > 0.5 {
			speed = fmt.Sprintf("· %s/s", HumanBytes(int64(rw.speed)))
		}
		if total > 0 {
			if done > total {
				done = total
			}
			pct := float64(done) * 100 / float64(total)
			eta := ""
			if rw.speed > 0.5 && pct < 100 {
				remaining := time.Duration(float64(total-done)/rw.speed) * time.Second
				eta = fmt.Sprintf("· reste %s", HumanDuration(remaining))
			}
			return fmt.Sprintf("%s %s %5.1f%%  %s/%s %s %s", base, bar(pct), pct, HumanBytes(done), HumanBytes(total), speed, eta)
		}
		return fmt.Sprintf("%s · taille inconnue · %s reçus %s", base, HumanBytes(done), speed)
	case store.StatusPaused:
		if rw.prog.Total > 0 {
			pct := float64(rw.prog.Done) * 100 / float64(rw.prog.Total)
			return fmt.Sprintf("%s %s %5.1f%%  %s/%s", base, bar(pct), pct, HumanBytes(rw.prog.Done), HumanBytes(rw.prog.Total))
		}
		return fmt.Sprintf("%s · %s reçus", base, HumanBytes(rw.prog.Done))
	case store.StatusDone:
		return fmt.Sprintf("%s · %s", base, HumanBytes(rw.prog.Total))
	case store.StatusFailed:
		return fmt.Sprintf("%s · %s", base, HumanBytes(rw.prog.Done))
	default: // en attente
		return base
	}
}

// Quiet imprime un dernier récapitulatif net au moment du départ.
func (r *Renderer) Quiet() {
	for {
		select {
		case p := <-r.ch:
			r.accept(p)
		default:
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.tty && r.lastLines > 0 {
				fmt.Fprintln(os.Stdout)
			}
			sorted := make([]string, len(r.ids))
			copy(sorted, r.ids)
			sort.SliceStable(sorted, func(i, j int) bool {
				return statRank(r.rows[sorted[i]].prog.Status) < statRank(r.rows[sorted[j]].prog.Status)
			})
			for _, id := range sorted {
				rw := r.rows[id]
				fmt.Fprintf(os.Stdout, "%-9s %-10s %s\n", fmt.Sprintf("#%s", Clip(id, 8)), StatLabel(rw.prog.Status), filepath.Base(rw.prog.Name))
			}
			return
		}
	}
}

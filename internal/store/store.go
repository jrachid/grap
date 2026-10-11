// Package store garde sur disque l'état des téléchargements (les morceaux déjà écrits),
// pour qu'un téléchargement interrompu reprenne à l'endroit exact où il s'était arrêté.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Les états possibles d'un téléchargement ; les couleurs d'écran s'y attachent dans ui.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusPaused  = "paused"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Un morceau d'un fichier, découpé pour être téléchargé en parallèle.
// Chunk : un morceau d'un fichier, découpé pour être téléchargé en parallèle ;
// pour les flux segmentés (HLS), un morceau = une tranche du flux avec son adresse propre.
type Chunk struct {
	Start   int64  `json:"start"`
	End     int64  `json:"end"` // inclusif ; -1 quand la taille du fichier est inconnue
	Written int64  `json:"written"`
	Done    bool   `json:"done"`
	URL     string `json:"url,omitempty"` // adresse propre des tranches de flux segmentés
}

// Length renvoie la taille du morceau.
func (c Chunk) Length() int64 {
	if c.End < c.Start {
		return 0
	}
	return c.End - c.Start + 1
}

// Options : choix mémorisé avec chaque téléchargement, pour la reprise dans une autre session.
type Options struct {
	Dir      string `json:"dir"`
	Segments int    `json:"segments"`
}

// Download : tout ce qu'il faut pour reprendre un téléchargement interrompu.
type Download struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Name       string     `json:"name"`
	Size       int64      `json:"size"` // -1 quand la taille est inconnue
	Ranged     bool       `json:"ranged"`
	Status     string     `json:"status"`
	Chunks     []Chunk    `json:"chunks"`
	Options    Options    `json:"options"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// BytesDone renvoie le nombre d'octets déjà écrits sur le disque.
func (d *Download) BytesDone() int64 {
	var n int64
	for _, c := range d.Chunks {
		n += c.Written
	}
	return n
}

// Path renvoie la destination finale du fichier.
func (d *Download) Path() string { return filepath.Join(d.Options.Dir, d.Name) }

// PartPath renvoie le fichier incomplet en cours de remplissage.
func (d *Download) PartPath() string { return d.Path() + ".grabpart" }

// Progress : un instantané envoyé par le moteur à l'écran.
type Progress struct {
	ID     string
	Name   string
	Status string
	Done   int64
	Total  int64 // -1 quand la taille est inconnue
}

// Store stocke et enregistre la liste des téléchargements dans ~/.grab/state.json.
type Store struct {
	mu        sync.Mutex
	path      string
	downloads []*Download
}

// Open lit l'état sur disque, ou démarre vide au premier lancement.
func Open() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".grab")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	raw, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.downloads); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Save écrit la liste entière sur disque de façon atomique (jamais un fichier à moitié écrit).
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.downloads, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Snapshot renvoie une copie de la liste, dans l'ordre d'ajout.
func (s *Store) Snapshot() []Download {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Download, 0, len(s.downloads))
	for _, d := range s.downloads {
		out = append(out, *d)
	}
	return out
}

// Add enregistre un nouveau téléchargement et lui attribue un identifiant.
func (s *Store) Add(d *Download) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.ID = s.newIDLocked()
	s.downloads = append(s.downloads, d)
	s.now(d)
	return s.saveLocked()
}

// Update remplace l'état d'un téléchargement existant (moteur : enregistrement de l'avancement).
func (s *Store) Update(d *Download) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findLocked(d.ID) == nil {
		return os.ErrNotExist
	}
	cp := *d
	*s.findLocked(d.ID) = cp
	s.now(s.findLocked(d.ID))
	return s.saveLocked()
}

// Get renvoie une copie du téléchargement ; le moteur en fait sa copie de travail.
func (s *Store) Get(id string) (Download, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.findLocked(id)
	if d == nil {
		return Download{}, false
	}
	return *d, true
}

// FindByID courte un identifiant demandé (4+ caractères) vers l'unique téléchargement qui colle.
func (s *Store) FindByPrefix(prefix string) (*Download, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *Download
	for _, d := range s.downloads {
		if len(d.ID) >= len(prefix) && d.ID[:len(prefix)] == prefix {
			if found != nil {
				return nil, errors.New("identifiant ambigu : donne plus de caractères")
			}
			found = d
		}
	}
	if found == nil {
		return nil, errors.New("aucun téléchargement de cet identifiant")
	}
	cp := *found
	return &cp, nil
}

// ActiveByURLOff retrouve un téléchargement pas encore terminé portant la même adresse, utile pour ne pas dédoublonner.
func (s *Store) ActiveByURL(url string) (*Download, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.downloads {
		if d.URL == url && d.Status != StatusDone && d.Status != StatusFailed {
			cp := *d
			return &cp, true
		}
	}
	return nil, false
}

// NextQueued renvoie une copie du premier téléchargement en attente, ou rien si la file est vide.
func (s *Store) NextQueued() *Download {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.downloads {
		if d.Status == StatusQueued {
			cp := *d
			return &cp
		}
	}
	return nil
}

// NewDownload prépare un téléchargement prêt à entrer dans la file ; le nom est déduit de l'adresse.
func NewDownload(raw, dir string, segments int) (*Download, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("adresse pas utilisable : %s", raw)
	}
	return &Download{
		URL:       raw,
		Name:      nameFromURL(u),
		Size:      -1,
		Status:    StatusQueued,
		Options:   Options{Dir: dir, Segments: segments},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil
}

// nameFromURL déduit un nom de fichier de l'adresse téléchargée.
func nameFromURL(u *url.URL) string {
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return "download"
	}
	return sanitizeName(base)
}

// sanitizeName neutralise les caractères interdits dans un nom de fichier.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "" {
		return "download"
	}
	if len(name) > 150 {
		ext := filepath.Ext(name)
		name = name[:150-len(ext)] + ext
	}
	return name
}

// Claim réserve un téléchargement pour l'exécution : passe de file ou pause à en cours,
// atomiquement ; renvoie faux si quelqu'un l'a déjà réservé (deux processus ne partent pas sur le même fichier).
func (s *Store) Claim(id string) (Download, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.findLocked(id)
	if d == nil || (d.Status != StatusQueued && d.Status != StatusPaused) {
		return Download{}, false
	}
	d.Status = StatusRunning
	d.Error = ""
	s.now(d)
	return *d, true
}

// Remove efface un téléchargement de la liste (et nettoie son fichier incomplet ; à faire par l'appelant).
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.downloads {
		if d.ID == id {
			s.downloads = append(s.downloads[:i], s.downloads[i+1:]...)
			return s.saveLocked()
		}
	}
	return nil
}

// ClearHistory efface téléchargements terminés ou échoués ; les autres restent.
func (s *Store) ClearHistory() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []*Download
	for _, d := range s.downloads {
		if d.Status != StatusDone && d.Status != StatusFailed {
			kept = append(kept, d)
		}
	}
	s.downloads = kept
	return s.saveLocked()
}

func (s *Store) findLocked(id string) *Download {
	for _, d := range s.downloads {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (s *Store) newIDLocked() string {
	for {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		if s.findLocked(id) == nil {
			return id
		}
	}
}

func (s *Store) now(d *Download) { d.UpdatedAt = time.Now() }

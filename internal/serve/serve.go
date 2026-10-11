// Package serve écoute les requêtes de l'extension navigateur sur le port local,
// fait entrer les adresses dans la file de grab et déroule cette file en fond.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"grab/internal/engine"
	"grab/internal/store"
	"grab/internal/ui"
)

// DefaultPort : port d'écoute du récepteur local utilisé par l'extension navigateur.
const DefaultPort = 48113

// tokenPath : le jeton partagé avec l'extension, rangé dans ~/.grab avec des droits d'accès restreints.
func tokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".grab", "serve-token"), nil
}

// loadToken lit le jeton partagé, ou en crée un au premier lancement.
func loadToken() (string, error) {
	path, err := tokenPath()
	if err != nil {
		return "", err
	}
	if raw, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(raw))
		if len(tok) >= 16 {
			return tok, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// Server porte le récepteur HTTP, l'état, le moteur et le jeton.
type Server struct {
	db    *store.Store
	eng   *engine.Engine
	token string

	logMu sync.Mutex
	last  map[string]string // identifiant → dernier état annoncé, pour n'imprimer que le changement
}

// AddRequest : ce que l'extension envoie quand un bouton est cliqué.
type AddRequest struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"` // nom de fichier proposé, sinon déduit de l'adresse
}

type addResponse struct {
	ID      string `json:"id"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]any{"ok": true, "version": "0.2.0"})
}

// add : ajoute une adresse à la file. Garde-barrière en deux volets :
// origin d'extension (les sites web ordinaires sont passés d'office), puis jeton secret.
func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, addResponse{Error: "méthode pas autorisée, veut POST"})
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && !strings.HasPrefix(origin, "chrome-extension://") {
		respond(w, http.StatusForbidden, addResponse{Error: "origin refusée : seule l'extension grab atteint ce port"})
		return
	}
	tok := r.Header.Get("X-Grab-Token")
	if tok == "" || tok != s.token {
		respond(w, http.StatusUnauthorized, addResponse{Error: "jeton manquant ou faux : colle celui de `grab serve` dans l'extension"})
		return
	}
	var req AddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, http.StatusBadRequest, addResponse{Error: "corps illisible : JSON voulu avec une url"})
		return
	}
	if existing, ok := s.db.ActiveByURL(req.URL); ok {
		respond(w, http.StatusOK, addResponse{ID: existing.ID, Warning: "déjà dans la file"})
		return
	}
	d, err := store.NewDownload(req.URL, s.dirFor(), 0)
	if err != nil {
		respond(w, http.StatusBadRequest, addResponse{Error: err.Error()})
		return
	}
	// 0 sert de code « valeur par défaut » ; le plan des morceaux s'en trouve choisi au moment de la sonde
	if d.Options.Segments <= 0 || d.Options.Segments > 32 {
		d.Options.Segments = 8
	}
	if req.Name != "" && validName(req.Name) {
		d.Name = req.Name
	}
	if err := s.db.Add(d); err != nil {
		respond(w, http.StatusInternalServerError, addResponse{Error: err.Error()})
		return
	}
	// file déjà vide : démarre sans attendre, sinon le dérouleur s'en chargera tout seul
	if s.eng.Idle() {
		s.eng.Start(d.ID)
	}
	respond(w, http.StatusOK, addResponse{ID: d.ID})
}

// dirFor donne le dossier de destination : celui du premier téléchargement déjà connu, sinon ~/Downloads.
func (s *Server) dirFor() string {
	for _, existing := range s.db.Snapshot() {
		if existing.Status != store.StatusFailed {
			d := existing.Options.Dir
			if d != "" {
				return d
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// validName limite les noms proposés de l'extension aux adjectifs sûr-sérieux.
func validName(name string) bool {
	if name == "" || len(name) > 160 || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	return true
}

// announce retranscrit les changements d'état dans la console, une ligne par changement,
// en ignorant le tickerrage de l'enregistreur périodique.
func (s *Server) announce(p store.Progress) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.last[p.ID] == p.Status {
		return
	}
	s.last[p.ID] = p.Status
	fmt.Printf("#%s %s — %s\n", p.ID, ui.StatLabel(p.Status), p.Name)
}

// loop déroule la file : dès qu'il n'y a plus rien en cours, il envoie le premier téléchargement en attente.
func (s *Server) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !s.eng.Idle() {
			continue
		}
		next := s.db.NextQueued()
		if next == nil {
			continue
		}
		s.eng.Start(next.ID)
	}
}

// Run démarre l'écoute du récepteur et le dérouleur de file, puis renvoie le jeton à donner à l'extension.
func Run(ctx context.Context, db *store.Store, eng *engine.Engine, port int, log func(store.Progress)) (string, error) {
	token, err := loadToken()
	if err != nil {
		return "", fmt.Errorf("jeton impossible : %w", err)
	}
	s := &Server{db: db, eng: eng, token: token, last: make(map[string]string)}
	// l'écran console : réutilisation directe si la commande en fournit un, sinon une ligne par changement d'état
	if log != nil {
		eng.Reporter = log
	} else {
		eng.Reporter = s.announce
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.ping)
	mux.HandleFunc("/add", s.add)
	srv := &http.Server{Addr: "127.0.0.1:" + strconv.Itoa(port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go s.loop(ctx)

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return "", fmt.Errorf("port %d occupé : le récepteur tourne-t-il déjà ailleurs ? (%v)", port, err)
	}
	fmt.Printf("grab serve — récepteur sur http://127.0.0.1:%d (Ctrl+C pour arrêter, en pause propre des téléchargements)\n", port)
	fmt.Println("jeton à coller dans l'extension : " + token)

	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return token, err
	}
	return token, nil
}

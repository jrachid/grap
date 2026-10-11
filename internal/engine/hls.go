// Package engine : les flux segmentés (HLS). Un lecteur web ne sert pas un gros fichier :
// il suit une playlist qui liste de petites tranches à charger une à une. grab lit la
// playlist (d'abord la liste des qualités s'il y en a), télécharge chaque tranche dans
// l'ordre, recoud le tout dans le fichier final ; une pause garde ce qui est déjà écrit,
// et une reprise repart à la tranche exacte où ça s'était arrêté.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"grab/internal/store"
)

const (
	playlistLimit = 2 << 20 // une playlist dépasse rarement 1 Mo ; au-delà, quelque chose cloche
	hlsHopMax     = 2       // listes de qualités (master → média) : deux descentes suffisent
)

// parseError désigne une playlist à laquelle on ne comprend rien.
type parseError struct{ err error }

func (pe parseError) Error() string { return pe.err.Error() }
func (pe parseError) Unwrap() error { return pe.err }

// errIsPlaylist : ce que le serveur a répondu n'est pas un fichier, c'est un playlist à suivre.
var errIsPlaylist = errors.New("ce contenu est une playlist à suivre, pas un fichier")

// segError : une panne attachée à une tranche ; fatal décide si on réessaie ou pas.
type segError struct {
	fatal bool
	err   error
}

func (e segError) Error() string { return e.err.Error() }
func (e segError) Unwrap() error { return e.err }

// isHLSURL dit si l'adresse ressemble à un playlist HLS.
func isHLSURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(path.Ext(u.Path), ".m3u8")
}

// findFFmpeg cherche ffmpeg dans le PATH, puis aux emplacements habituels de Mac (Homebrew et compères).
func findFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, cand := range []string{
		"/opt/homebrew/bin/ffmpeg",
		"/usr/local/bin/ffmpeg",
		"/usr/bin/ffmpeg",
	} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// remuxHLS : recopie le flux .ts finalisé dans un conteneur .mp4, sans réencoder (aucune perte),
// pour que QuickTime le joue aussi. ffmpeg doit exister sur la machine ; sans lui, le .ts reste tel quel.
func (e *Engine) remuxHLS(d *store.Download) error {
	ffmpeg := findFFmpeg()
	if ffmpeg == "" {
		return errors.New("ffmpeg introuvable (ni dans le PATH, ni dans /opt/homebrew/bin) : le .ts reste téléchargé tel quel")
	}
	source := d.Path()
	target := filepath.Join(d.Options.Dir, strings.TrimSuffix(d.Name, filepath.Ext(d.Name))+".mp4")
	if _, err := os.Stat(target); err == nil {
		target = filepath.Join(d.Options.Dir, uniqueFileName(d.Options.Dir, strings.TrimSuffix(d.Name, filepath.Ext(d.Name))+".mp4"))
	}
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-i", source, "-c", "copy", "-movflags", "+faststart", target)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("recopie MP4 impossible : %v : %s", err, tail(string(out)))
	}
	_ = os.Remove(source)
	e.dmu.Lock()
	d.Name = filepath.Base(target)
	if st, serr := os.Stat(target); serr == nil {
		d.Size = st.Size()
	}
	e.dmu.Unlock()
	return nil
}

// tail ramène la fin d'une sortie, pour l'honnêteté des messages de commande.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

// maybeRemux : la remuxion ne concerne que les flux dont les tranches sont du .ts ; un .mp4 natif reste tel quel.
func (e *Engine) maybeRemux(d *store.Download) {
	hls := len(d.Chunks) > 0 && d.Chunks[0].URL != ""
	if !hls || !strings.EqualFold(filepath.Ext(d.Name), ".ts") {
		return
	}
	if err := e.remuxHLS(d); err != nil {
		fmt.Fprintf(os.Stderr, "grab: %v\n", err)
	}
}

// resolveURL assemble une adresse relative de playlist avec l'adresse du playlist lui-même.
func resolveURL(base *url.URL, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	if base == nil {
		return ref
	}
	resolved, err := base.Parse(ref)
	if err != nil {
		return ref
	}
	return resolved.String()
}

// fetchText télécharge un playlist en entier ( borne généreuse, c'est du texte ).
func (e *Engine) fetchText(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("adresse pas utilisable : %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := e.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("connexion impossible : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("le serveur a répondu HTTP %d pour la playlist", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, playlistLimit))
	if err != nil {
		return "", fmt.Errorf("lecture du playlist impossible : %w", err)
	}
	return string(raw), nil
}

// hlsMedia : le contenu d'une playlist média, une fois les qualités tranchées.
type hlsMedia struct {
	initURI    string // tranche d'initialisation, toujours la première pièce
	segments   []string
	encrypted  bool // méthode de chiffrement pas nulle : hors de portée pour l'instant
	hasContent bool
}

// parseMedia lit une playlist média : tranches chiffrées refusées, le reste est servi.
func parseMedia(text string, base *url.URL) (hlsMedia, error) {
	var m hlsMedia
	if !strings.Contains(text, "#EXTM3U") {
		return m, parseError{errors.New("la réponse n'est pas une playlist HLS (balise #EXTM3U absente)")}
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") && !strings.Contains(strings.ToUpper(line), "METHOD=NONE") {
			m.encrypted = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:URI=") {
			uri := strings.Trim(strings.TrimPrefix(line, "#EXT-X-MAP:URI="), `"'`)
			m.initURI = resolveURL(base, uri)
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			return m, parseError{errors.New("la playlist annonce encore des qualités : pas de tranches ici")}
		}
		if !strings.HasPrefix(line, "#") {
			uri := resolveURL(base, line)
			if uri != "" {
				m.segments = append(m.segments, uri)
			}
		}
	}
	m.hasContent = len(m.segments) > 0 || m.initURI != ""
	if !m.hasContent {
		return m, parseError{errors.New("la playlist ne contient aucune tranche")}
	}
	return m, nil
}

// pickBestVariant lit une playlist de qualités (master) et renvoie l'adresse de la plus riche en débit.
func pickBestVariant(text string, base *url.URL) (string, error) {
	if !strings.Contains(text, "#EXTM3U") {
		return "", parseError{errors.New("la réponse n'est pas une playlist HLS")}
	}
	if !strings.Contains(text, "#EXT-X-STREAM-INF") {
		return "", nil // ce n'était pas une liste de qualités ; appelant passera au média
	}
	best, bestBW := "", int64(-1)
	pending := int64(-1)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pending = 0
			for _, attr := range strings.Split(line, ",") {
				kv := strings.SplitN(strings.TrimSpace(attr), "=", 2)
				if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "BANDWIDTH") {
					n, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
					if err == nil && n > pending {
						pending = n
					}
				}
			}
		case line != "" && !strings.HasPrefix(line, "#") && pending >= 0:
			if pending > bestBW {
				bestBW = pending
				best = resolveURL(base, line)
			}
			pending = -1
		}
	}
	if best == "" {
		return "", parseError{errors.New("playlist de qualités illisible : aucune tranche trouvée")}
	}
	return best, nil
}

// prepareHLS sonde le flux : chaîne master → média, puis découpe en morceaux, une adresse par tranche.
func (e *Engine) prepareHLS(ctx context.Context, d *store.Download) error {
	target := d.URL
	var m hlsMedia
	for hopCount := 0; hopCount <= hlsHopMax; hopCount++ {
		text, err := e.fetchText(ctx, target)
		if err != nil {
			return err
		}
		base, err := url.Parse(target)
		if err != nil {
			return fmt.Errorf("adresse de playlist pas utilisable : %w", err)
		}
		variant, verr := pickBestVariant(text, base)
		if verr != nil {
			return verr
		}
		if variant != "" {
			// liste de qualités : on suit celle qui promet le plus de débit
			target = variant
			continue
		}
		m, err = parseMedia(text, base)
		if err != nil {
			return err
		}
		if m.encrypted {
			return errors.New("flux HLS chiffré (AES) : pas encore pris en charge")
		}
		break
	}
	if !m.hasContent {
		return parseError{errors.New("playlist vide : aucune tranche à télécharger")}
	}
	// nom final : l'extension de la première tranche (.ts ou .mp4 en général), débarrassée du .m3u8
	ext := ""
	if len(m.segments) > 0 {
		ext = fileExtOf(m.segments[0])
	}
	baseName := d.Name
	if baseName == "" {
		baseName = "video"
	}
	trimmed := strings.TrimSuffix(baseName, ".m3u8")
	d.Name = trimmed + ext

	var chunks []store.Chunk
	if m.initURI != "" {
		chunks = append(chunks, store.Chunk{URL: m.initURI})
	}
	for _, seg := range m.segments {
		chunks = append(chunks, store.Chunk{URL: seg})
	}
	d.Chunks = chunks
	d.Ranged = false
	d.Size = -1
	return nil
}

// fetchHLS télécharge les tranches dans l'ordre ; en cas de panne, il repart à la tranche ratée,
// ce qui complète exactement ce qui est déjà écrit au fichier final.
func (e *Engine) fetchHLS(ctx context.Context, d *store.Download, file *os.File) error {
	for idx := range d.Chunks {
		if ctx.Err() != nil {
			return ErrStopped
		}
		c := &d.Chunks[idx]
		e.dmu.Lock()
		done := c.Done
		e.dmu.Unlock()
		if done {
			continue
		}
		back := newBackoff()
		var lastErr error
		for attempt := 0; attempt < maxRetries; attempt++ {
			// une tranche ratée repart de zéro
			e.dmu.Lock()
			c.Written = 0
			e.dmu.Unlock()
			err := e.fetchHLSSegment(ctx, d, file, c)
			if err == nil {
				lastErr = nil
				break
			}
			if ctx.Err() != nil {
				return ErrStopped
			}
			var se segError
			if errors.As(err, &se) && se.fatal {
				return fetchErr{c, se}
			}
			lastErr = err
			dur, ok := back.next()
			if !ok {
				return fetchErr{c, err}
			}
			if !sleepCtx(ctx, dur) {
				return ErrStopped
			}
		}
		if lastErr != nil {
			return fetchErr{c, lastErr}
		}
	}
	return nil
}

// fileExtOf renvoie l'extension d'un fichier cible (avec le point), vide en cas de doute.
func fileExtOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return path.Ext(u.Path)
}

// offsetBefore : le nombre d'octets déjà écrits par toutes les tranches précédentes,
// c'est la position du coin d'un courant dans le fichier final.
func offsetBefore(d *store.Download, idx int) int64 {
	var offset int64
	for i := 0; i < idx; i++ {
		offset += d.Chunks[i].Written
	}
	return offset
}

// fetchHLSSegment télécharge une tranche du flux en entier, et l'ajoute au fichier final à sa place.
func (e *Engine) fetchHLSSegment(ctx context.Context, d *store.Download, file *os.File, c *store.Chunk) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return segError{fatal: true, err: fmt.Errorf("adresse de tranche pas utilisable : %w", err)}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("connexion interrompue : %w", err)
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		// ok
	case status >= 400 && status < 500:
		return segError{fatal: true, err: fmt.Errorf("le serveur a refusé la tranche (HTTP %d)", status)}
	default:
		return segError{fatal: false, err: fmt.Errorf("panne du serveur pour la tranche (HTTP %d)", status)}
	}

	idx := 0
	for i := range d.Chunks {
		if &d.Chunks[i] == c {
			idx = i
			break
		}
	}
	at := offsetBefore(d, idx)
	buf := make([]byte, 1<<16)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := file.WriteAt(buf[:n], at); werr != nil {
				return fmt.Errorf("écriture disque impossible : %w", werr)
			}
			e.dmu.Lock()
			c.Written += int64(n)
			e.dmu.Unlock()
			at += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("flux interrompu au milieu de la tranche : %w", rerr)
		}
	}
	e.dmu.Lock()
	c.Written = at - offsetBefore(d, idx)
	c.Done = true
	e.dmu.Unlock()
	return nil
}

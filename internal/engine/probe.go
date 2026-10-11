// Package engine : la sonde. On demande au serveur juste ce qu'il faut pour connaître
// la taille du fichier, s'il accepte les morceaux, et le nom qu'il propose.
package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"grab/internal/store"
)

// inspect sonde le serveur : taille totale, prise en charge des morceaux, nom de fichier proposé.
// Une adresse en .m3u8 (ou une réponse playlist) court-circuite elle : c'est un flux segmenté.
func (e *Engine) inspect(ctx context.Context, d *store.Download) error {
	if isHLSURL(d.URL) {
		return e.prepareHLS(ctx, d)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return fmt.Errorf("adresse invalide : %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")

	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("connexion impossible : %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch status := resp.StatusCode; status {
	case http.StatusPartialContent:
		d.Ranged = true
		_, total := parseContentRange(resp.Header.Get("Content-Range"))
		if total < 0 {
			return fmt.Errorf("le serveur promet des morceaux mais ne dit pas la taille du fichier")
		}
		d.Size = total
	case http.StatusOK:
		d.Ranged = false
		d.Size = resp.ContentLength // -1 quand la taille est inconnue
	case http.StatusNotFound:
		return fmt.Errorf("le fichier n'existe pas sur le serveur (404)")
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("accès refusé par le serveur (HTTP %d)", status)
	default:
		return fmt.Errorf("le serveur a répondu HTTP %d", status)
	}

	name := nameFromDisposition(resp.Header.Get("Content-Disposition"))
	if name == "" {
		name = baseNameFromURL(resp.Request.URL)
	}
	if name != "" {
		d.Name = name
	}
	return nil
}

// nameFromDisposition lit le nom proposé par le serveur dans l'entête Content-Disposition.
func nameFromDisposition(cd string) string {
	re := regexp.MustCompile(`filename\*?=(?:UTF-8''|")?([^";]+)`)
	m := re.FindStringSubmatch(cd)
	if len(m) < 2 {
		return ""
	}
	name := m[1]
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	return strings.TrimSpace(name)
}

// baseNameFromURL déduit un nom de fichier de l'adresse téléchargée.
func baseNameFromURL(u *url.URL) string {
	if u == nil || u.Path == "" || u.Path == "/" {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		return ""
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

// uniqueFileName renvoie un nom libre au cas où un fichier du même nom existerait déjà.
func uniqueFileName(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(filepath.Join(dir, cand)); os.IsNotExist(err) {
			return cand
		}
	}
}

// planChunks découpe la taille totale en morceaux à peu près égaux, sans découper les fichiers petits.
func (e *Engine) planChunks(total int64, segments int) []store.Chunk {
	if total <= 0 {
		return []store.Chunk{{Start: 0, End: -1}}
	}
	if segments < 1 {
		segments = defaultSegments
	}
	if segments > maxSegments {
		segments = maxSegments
	}
	const minSegmentSize = 512 << 10
	if biggest := int(total/minSegmentSize) + 1; biggest < segments {
		segments = biggest
	}
	if segments < 1 {
		segments = 1
	}
	per := total / int64(segments)
	chunks := make([]store.Chunk, 0, segments)
	var start int64
	for i := 0; i < segments; i++ {
		end := start + per - 1
		if i == segments-1 {
			end = total - 1
		}
		chunks = append(chunks, store.Chunk{Start: start, End: end})
		start = end + 1
	}
	return chunks
}

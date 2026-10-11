// Package engine : la découpe du fichier, les morceaux téléchargés en parallèle, et la couture finale.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"grab/internal/store"
	"grab/internal/ui"
)

const (
	maxRetries   = 20
	backoffStart = 1500 * time.Millisecond
	backoffMax   = 20 * time.Second
)

// errNeedFresh : le serveur a renvoyé le fichier entier au lieu du morceau demandé ; le lot repart de zéro.
var errNeedFresh = errors.New("server will not satisfy ranges")

func sleepCtx(ctx context.Context, dur time.Duration) bool {
	timer := time.NewTimer(dur)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// backoff attend de plus en plus longtemps entre deux tentatives, jusqu'à un plafond.
type backoff struct {
	n    int
	max  int
	wait time.Duration
}

func newBackoff() *backoff { return &backoff{max: maxRetries, wait: backoffStart} }

func (b *backoff) next() (time.Duration, bool) {
	b.n++
	if b.n > b.max {
		return 0, false
	}
	d := b.wait
	b.wait *= 2
	if b.wait > backoffMax {
		b.wait = backoffMax
	}
	return d, true
}

// rangedHeaderValue construit la valeur de l'entête Range pour les octets [from, end].
func rangedHeaderValue(from, end int64) string {
	if end < from {
		return fmt.Sprintf("bytes=%d-", from)
	}
	return fmt.Sprintf("bytes=%d-%d", from, end)
}

// fetchAllOnce lance tous les morceaux restants en simultané et attend le plus lent.
func (e *Engine) fetchAllOnce(ctx context.Context, d *store.Download, file *os.File) error {
	var wg sync.WaitGroup
	var firstErr atomic.Value // la première panne sérieuse du lot
	var needFresh atomic.Bool
	for i := range d.Chunks {
		c := &d.Chunks[i]
		e.dmu.Lock()
		done := c.Done
		e.dmu.Unlock()
		if done {
			continue
		}
		wg.Add(1)
		go func(c *store.Chunk) {
			defer wg.Done()
			err := e.fetchChunk(ctx, d, file, c)
			if err == nil {
				return
			}
			if errors.Is(err, errNeedFresh) {
				needFresh.Store(true)
				return
			}
			firstErr.CompareAndSwap(nil, err)
		}(c)
	}
	wg.Wait()
	if err := firstErr.Load(); err != nil {
		return err.(error)
	}
	if needFresh.Load() {
		return errNeedFresh
	}
	return nil
}

// fetchChunk télécharge un morceau du début à la fin, avec nouvelles tentatives en cas de panne réseau.
func (e *Engine) fetchChunk(ctx context.Context, d *store.Download, file *os.File, c *store.Chunk) error {
	if ctx.Err() != nil {
		return ErrStopped
	}
	back := newBackoff()
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := e.fetchOnce(ctx, d, file, c)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNeedFresh):
			return err
		case ctx.Err() != nil:
			return ErrStopped
		default:
			dur, ok := back.next()
			if !ok {
				return fetchErr{c, err}
			}
			if !sleepCtx(ctx, dur) {
				return ErrStopped
			}
		}
	}
	return fetchErr{c, errors.New("trop de tentatives infructueuses")}
}

// fetchErr : une panne attachée au morceau qui l'a soufferte.
type fetchErr struct {
	c  *store.Chunk
	of error
}

func (fe fetchErr) Error() string {
	if fe.c.End < 0 {
		return fmt.Sprintf("téléchargement : %v", fe.of)
	}
	return fmt.Sprintf("morceau %d-%d : %v", fe.c.Start, fe.c.End, fe.of)
}

func (fe fetchErr) Unwrap() error { return fe.of }

// fetchOnce fait une tentative de téléchargement d'un morceau : demande au serveur juste
// les octets qui manquent et les écrit à leur place dans le fichier incomplet.
func (e *Engine) fetchOnce(ctx context.Context, d *store.Download, file *os.File, c *store.Chunk) error {
	from := c.Start + c.Written
	if c.End >= 0 && from > c.End {
		e.dmu.Lock()
		c.Done = true
		e.dmu.Unlock()
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return fmt.Errorf("adresse pas utilisable : %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	if d.Ranged {
		req.Header.Set("Range", rangedHeaderValue(from, c.End))
	}

	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("connexion interrompue : %w", err)
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	switch {
	case status == http.StatusPartialContent && d.Ranged:
		start, _ := parseContentRange(resp.Header.Get("Content-Range"))
		if start >= 0 && start != from {
			return fmt.Errorf("le serveur sert la plage à partir de %d au lieu de %d", start, from)
		}
	case status == http.StatusOK && d.Ranged:
		return errNeedFresh
	case status == http.StatusOK && !d.Ranged:
		// tout-va-bien d'un téléchargement simple, sans plages d'octets
	case status >= 400 && status < 500:
		return fmt.Errorf("le serveur a répondu HTTP %d (%s)", status, http.StatusText(status))
	case status >= 500:
		return fmt.Errorf("panne du serveur (HTTP %d)", status)
	default:
		return fmt.Errorf("réponse inattendue du serveur (HTTP %d)", status)
	}

	// un lecteur peut déguiser son playlist sous une adresse de fichier : à ce point, on suit le liste
	if strings.Contains(resp.Header.Get("Content-Type"), "mpegurl") {
		return errIsPlaylist
	}

	fd := file
	if !d.Ranged {
		// sans reprise par plage, chaque tentative repart d'un fichier neuf
		f, oerr := os.OpenFile(d.PartPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if oerr != nil {
			return fmt.Errorf("fichier de travail inaccessible : %w", oerr)
		}
		defer f.Close()
		fd = f
	}

	var body io.Reader = resp.Body
	if remaining := c.Length() - c.Written; d.Ranged && remaining > 0 {
		body = io.LimitReader(resp.Body, remaining)
	}

	buf := make([]byte, 1<<16)
	at := from
	if !d.Ranged {
		// chaque tentative repart d'un fichier neuf : on écrit donc depuis le début du fichier
		at = 0
	}
	peek := !d.Ranged && (resp.Header.Get("Content-Type") == "" ||
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/"))
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if peek && looksLikePlaylist(buf[:n]) {
				return errIsPlaylist
			}
			peek = false
			if _, werr := fd.WriteAt(buf[:n], at); werr != nil {
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
			return fmt.Errorf("flux interrompu : %w", rerr)
		}
	}

	if d.Ranged && at != c.End+1 {
		return fmt.Errorf("le serveur a envoyé %d octets au lieu de %d pour le morceau", at-c.Start, c.Length())
	}
	if !d.Ranged && d.Size > 0 && at != d.Size {
		return fmt.Errorf("flux coupé avant la fin : %d octets reçus sur %s", at, ui.HumanBytes(d.Size))
	}
	e.dmu.Lock()
	c.Written = at - c.Start
	c.Done = true
	e.dmu.Unlock()
	return nil
}

// looksLikePlaylist : reconnait une playlist HLS qui s'est faufilée à la place d'un fichier.
func looksLikePlaylist(b []byte) bool {
	return len(b) >= 7 && strings.HasPrefix(string(b), "#EXTM3U")
}

// finalize scelle un téléchargement réussi : taille exacte, puis le fichier incomplet devient le fichier final.
func (e *Engine) finalize(d *store.Download, file *os.File) error {
	if d.Size > 0 {
		if err := file.Truncate(d.Size); err != nil {
			return fmt.Errorf("ajustement de la taille finale impossible : %w", err)
		}
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("synchronisation disque impossible : %w", err)
	}
	if _, err := os.Stat(d.Path()); err == nil {
		e.dmu.Lock()
		d.Name = uniqueFileName(d.Options.Dir, d.Name)
		e.dmu.Unlock()
	}
	if err := os.Rename(d.PartPath(), d.Path()); err != nil {
		return fmt.Errorf("déplacement du fichier fini impossible : %w", err)
	}
	if st, serr := os.Stat(d.Path()); serr == nil {
		e.dmu.Lock()
		d.Size = st.Size()
		e.dmu.Unlock()
	}
	return nil
}

// writeEmptyFile couvre le fichier de taille nulle renvoyé par le serveur.
func (e *Engine) writeEmptyFile(d *store.Download) error {
	if _, err := os.Stat(d.Path()); err == nil {
		e.dmu.Lock()
		d.Name = uniqueFileName(d.Options.Dir, d.Name)
		e.dmu.Unlock()
	}
	if err := os.MkdirAll(d.Options.Dir, 0o755); err != nil {
		return fmt.Errorf("dossier de destination inaccessible : %w", err)
	}
	return os.WriteFile(d.Path(), []byte{}, 0o644)
}

// parseContentRange lit « bytes 12-345/678 » et renvoie le début de la plage, la taille totale, ou -1 - -1.
func parseContentRange(h string) (start, total int64) {
	start, total = -1, -1
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(h), "bytes "), "/", 2)
	if len(parts) != 2 {
		return
	}
	tot, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || tot < 0 {
		return
	}
	rng := strings.TrimSpace(parts[0])
	dash := strings.IndexByte(rng, '-')
	if dash < 0 {
		return
	}
	sn, err := strconv.ParseInt(strings.TrimSpace(rng[:dash]), 10, 64)
	if err != nil {
		return
	}
	return sn, tot
}

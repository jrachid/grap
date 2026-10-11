// La commande grab télécharge des fichiers en morceaux parallèles, avec pause (Ctrl+C), reprise et file d'attente.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"grab/internal/engine"
	"grab/internal/serve"
	"grab/internal/store"
	"grab/internal/ui"
)

func main() {
	os.Exit(run())
}

func usage() {
	fmt.Print(`grab — télécharge en morceaux parallèles, avec pause et reprise (l'esprit IDM)

usage :
  grab [options] <adresse>…   télécharge maintenant
  grab add <adresse>…         ajoute à la file sans lancer
  grab go                     lance (et reprend) tout ce qui est en attente
  grab ls                     montre la file et les téléchargements récents
  grab history                montre les téléchargements terminés ou échoués
  grab pause <id>             met en pause un téléchargement en cours
  grab resume [id…]           reprend des téléchargements en pause
  grab rm <id>                abandonne un téléchargement incomplet
  grab clear                  vide l'historique
  grab serve [port]           récepteur local pour l'extension navigateur + file en continu (port 48113 par défaut)
  grab help                   cette aide

options :
  -d <dossier>                dossier de destination (par défaut : le dossier courant)
  -s <n>                      nombre de morceaux en parallèle, de 1 à 32 (par défaut : 8)

Pendant un téléchargement, Ctrl+C met en pause et conserve ce qui est déjà écrit :
relance ` + "`grab go`" + ` (ou la même commande) pour reprendre là où ça s'était arrêté.
Si le serveur accepte les plages d'octets, le fichier tombe en morceaux en parallèle ;
les flux vidéo par petites tranches (playlists m3u8) sont suivis et recousus dans l'ordre.
Les fichiers incomplets sont rangés à côté du fichier final, suffixés .grabpart.
Sur les flux .ts, si ffmpeg est présent, le fichier est recopié en .mp4 (sans réencoder) et le .ts intermédiaire est effacé.

Pour la barre d'outils du navigateur : lance ` + "`grab serve`" + `, puis installe l'extension
du dossier ` + "`extension/`" + ` dans chrome://extensions (mode développeur), et colle le jeton
affiché par grab serve dans la pop-up de l'extension.
`)
}

func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

func run() int {
	args := os.Args[1:]
	dirFlag, segsFlag, rest, ferr := splitFlags(args)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "grab: %v\n", ferr)
		return 2
	}
	if segsFlag < 1 || segsFlag > 32 {
		fmt.Fprintln(os.Stderr, "grab: le nombre de morceaux doit être entre 1 et 32")
		return 2
	}
	dirFlag = expandPath(dirFlag)

	db, err := store.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "grab: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dirFlag, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "grab: dossier inaccessible : %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rend := ui.New()
	go rend.Serve(ctx)
	defer rend.Quiet()
	eng := engine.New(ctx, db, rend.Push)

	cmd := ""
	if len(rest) > 0 && knownCommand(rest[0]) {
		cmd, rest = rest[0], rest[1:]
	}

	switch {
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		usage()
		return 0
	case cmd == "version":
		fmt.Println("grab 0.1.0")
		return 0
	case cmd == "add":
		return cmdAdd(db, dirFlag, segsFlag, rest)
	case cmd == "go":
		return cmdRun(ctx, db, eng, rest)
	case cmd == "ls":
		return cmdLs(db)
	case cmd == "history":
		return cmdHistory(db)
	case cmd == "pause":
		return cmdPause(db, eng, rest)
	case cmd == "resume":
		return cmdResume(ctx, db, eng, rest)
	case cmd == "rm":
		return cmdRm(db, eng, rest)
	case cmd == "clear":
		return cmdClear(db)
	case cmd == "serve":
		return cmdServe(ctx, db, eng, rest)
	default:
		if len(rest) == 0 || !strings.Contains(rest[0], "://") {
			usage()
			return 2
		}
		return cmdDirect(ctx, db, eng, dirFlag, segsFlag, rest)
	}
}

// splitFlags capte -d/-s où qu'ils apparaissent, et laisse le reste in-extenso.
func splitFlags(args []string) (string, int, []string, error) {
	dir := defaultDir()
	segs := 8
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-d", "--dir":
			if i+1 >= len(args) {
				return "", 0, nil, errors.New("-d attend un dossier après lui")
			}
			i++
			dir = args[i]
		case "-s", "-p", "--segments":
			if i+1 >= len(args) {
				return "", 0, nil, errors.New("-s attend un nombre après lui")
			}
			i++
			n, aerr := strconv.Atoi(args[i])
			if aerr != nil {
				return "", 0, nil, fmt.Errorf("-s attend un nombre, pas %s", args[i])
			}
			segs = n
		default:
			rest = append(rest, arg)
		}
	}
	return dir, segs, rest, nil
}

// knownCommand dit si c'est une sous-commande ou une adresse passée directement.
func knownCommand(head string) bool {
	switch head {
	case "add", "go", "ls", "history", "pause", "resume", "rm", "clear", "version", "help", "serve":
		return true
	}
	return false
}

// cmdServe lance le récepteur local pour l'extension navigateur et déroule la file en continu.
func cmdServe(ctx context.Context, db *store.Store, eng *engine.Engine, args []string) int {
	port := serve.DefaultPort
	if len(args) > 1 {
		usage()
		return 2
	}
	if len(args) == 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 || n > 65535 {
			fmt.Fprintf(os.Stderr, "grab: port pas utilisable : %s\n", args[0])
			return 2
		}
		port = n
	}
	if _, err := serve.Run(ctx, db, eng, port, nil); err != nil {
		fmt.Fprintf(os.Stderr, "grab: %v\n", err)
		return 1
	}
	return 0
}

// cmdDirect : on donne directement des adresses en ligne ; elles sont mises en file puis lancées.
func cmdDirect(ctx context.Context, db *store.Store, eng *engine.Engine, dir string, segs int, args []string) int {
	ids := make([]string, 0, len(args))
	for _, raw := range args {
		if existing, ok := db.ActiveByURL(raw); ok {
			fmt.Printf("#%s %s — déjà dans la file, on passe\n", existing.ID, existing.Name)
			ids = append(ids, existing.ID)
			continue
		}
		d, err := store.NewDownload(raw, dir, segs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			continue
		}
		if err := db.Add(d); err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			return 1
		}
		ids = append(ids, d.ID)
	}
	return cmdRun(ctx, db, eng, ids)
}

// cmdAdd : mise en file sans lancement.
func cmdAdd(db *store.Store, dir string, segs int, args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	for _, raw := range args {
		if existing, ok := db.ActiveByURL(raw); ok {
			fmt.Printf("#%s %s — déjà dans la file, on passe\n", existing.ID, existing.Name)
			continue
		}
		d, err := store.NewDownload(raw, dir, segs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			continue
		}
		if err := db.Add(d); err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			return 1
		}
		fmt.Printf("#%s %s — mis en file\n", d.ID, d.Name)
	}
	fmt.Println("lance `grab go` pour démarrer")
	return 0
}

// cmdRun démarre la filé indiquée (ou toute la file) et attend que ce soit au bout.
func cmdRun(ctx context.Context, db *store.Store, eng *engine.Engine, ids []string) int {
	if len(ids) == 0 {
		for _, d := range db.Snapshot() {
			if d.Status == store.StatusQueued || d.Status == store.StatusPaused {
				ids = append(ids, d.ID)
			}
		}
	}
	for _, id := range ids {
		d, ok := db.Get(id)
		if !ok {
			fmt.Fprintf(os.Stderr, "grab: téléchargement introuvable : %s\n", id)
			continue
		}
		if d.Status == store.StatusPaused {
			d.Status = store.StatusQueued
			if err := db.Update(&d); err != nil {
				fmt.Fprintf(os.Stderr, "grab: %v\n", err)
				continue
			}
		}
		if d.Status != store.StatusQueued {
			continue
		}
		if !eng.Start(id) {
			fmt.Printf("#%s déjà en cours\n", id)
		}
	}
	eng.Wait()

	failed := 0
	for _, d := range db.Snapshot() {
		if d.Status == store.StatusFailed {
			failed++
		}
	}
	if ctx.Err() != nil {
		fmt.Println("\nMis en pause avec Ctrl+C — ce qui est déjà écrit est conservé.")
		fmt.Println("Relance `grab go` pour reprendre ces téléchargements.")
		return 130
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d téléchargement(s) en échec — `grab history` pour le détail, `grab resume` pour retenter.\n", failed)
		return 1
	}
	return 0
}

// cmdLs montre la file et les téléchargements récents.
func cmdLs(db *store.Store) int {
	all := db.Snapshot()
	if len(all) == 0 {
		fmt.Println("file vide")
		return 0
	}
	fmt.Printf("%-10s %-10s %-30s %s\n", "ID", "ÉTAT", "FICHIER", "PROGRÈS")
	for _, d := range all {
		fmt.Printf("#%-9s %-10s %-30s %s\n", d.ID, ui.StatLabel(d.Status), ui.Clip(d.Name, 28), progressFor(&d))
	}
	return 0
}

// cmdHistory suffit à l'affichage des téléchargements terminés ou échoués.
func cmdHistory(db *store.Store) int {
	var finis []store.Download
	for _, d := range db.Snapshot() {
		if d.Status == store.StatusDone || d.Status == store.StatusFailed {
			finis = append(finis, d)
		}
	}
	if len(finis) == 0 {
		fmt.Println("historique vide")
		return 0
	}
	fmt.Printf("%-10s %-8s %-30s %10s %12s\n", "ID", "ÉTAT", "FICHIER", "TAILLE", "FIN")
	for _, d := range finis {
		at := "-"
		if d.FinishedAt != nil {
			at = d.FinishedAt.Format("02/01 15:04")
		}
		fmt.Printf("#%-9s %-8s %-30s %10s %12s\n", d.ID, ui.StatLabel(d.Status), ui.Clip(d.Name, 28), ui.HumanBytes(d.Size), at)
	}
	return 0
}

// cmdPause met en pause un téléchargement (en cours, ou même pas encore démarré).
func cmdPause(db *store.Store, eng *engine.Engine, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "grab: usage : grab pause <id>…")
		return 2
	}
	for _, arg := range args {
		d, err := db.FindByPrefix(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			continue
		}
		switch d.Status {
		case store.StatusRunning:
			eng.Stop(d.ID)
			fmt.Printf("#%s %s — pause demandée\n", d.ID, d.Name)
		case store.StatusQueued:
			d.Status = store.StatusPaused
			d.Error = ""
			_ = db.Update(d)
			fmt.Printf("#%s %s — mis en pause (il n'avait pas encore démarré)\n", d.ID, d.Name)
		case store.StatusPaused:
			fmt.Printf("#%s %s — déjà en pause\n", d.ID, d.Name)
		default:
			fmt.Printf("#%s %s — %s, rien à mettre en pause\n", d.ID, d.Name, ui.StatLabel(d.Status))
		}
	}
	return 0
}

// cmdResume relance des téléchargements en pause, ou tous si aucun identifiant n'est donné.
func cmdResume(ctx context.Context, db *store.Store, eng *engine.Engine, args []string) int {
	ids := make([]string, 0, len(args))
	if len(args) == 0 {
		for _, d := range db.Snapshot() {
			if d.Status == store.StatusPaused || d.Status == store.StatusFailed {
				ids = append(ids, d.ID)
			}
		}
	} else {
		for _, arg := range args {
			d, err := db.FindByPrefix(arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "grab: %v\n", err)
				continue
			}
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		fmt.Println("rien à reprendre")
		return 0
	}
	return cmdRun(ctx, db, eng, ids)
}

// cmdRm abandonne un téléchargement : son fichier incomplet disparaît, le fichier fini reste tel quel.
func cmdRm(db *store.Store, eng *engine.Engine, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "grab: usage : grab rm <id>")
		return 2
	}
	for _, arg := range args {
		d, err := db.FindByPrefix(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			continue
		}
		if d.Status == store.StatusRunning {
			eng.Stop(d.ID)
			fmt.Printf("#%s %s — arrêt demandé, relance `grab rm <id>` quand la pause est effective\n", d.ID, d.Name)
			continue
		}
		_ = os.Remove(d.PartPath())
		if err := db.Remove(d.ID); err != nil {
			fmt.Fprintf(os.Stderr, "grab: %v\n", err)
			continue
		}
		fmt.Printf("#%s %s — abandonné\n", d.ID, d.Name)
	}
	return 0
}

// cmdClear vide l'historique des téléchargements finis ou échoués.
func cmdClear(db *store.Store) int {
	if err := db.ClearHistory(); err != nil {
		fmt.Fprintf(os.Stderr, "grab: %v\n", err)
		return 1
	}
	fmt.Println("historique vidé")
	return 0
}

// progressFor dépeint l'avancement d'un téléchargement pour les listes.
func progressFor(d *store.Download) string {
	done := d.BytesDone()
	if d.Size > 0 {
		if done > d.Size {
			done = d.Size
		}
		pct := float64(done) * 100 / float64(d.Size)
		return fmt.Sprintf("%5.1f%%  %s/%s", pct, ui.HumanBytes(done), ui.HumanBytes(d.Size))
	}
	if d.Status == store.StatusDone {
		return "taille inconnue"
	}
	return fmt.Sprintf("%s reçus", ui.HumanBytes(done))
}

// expandPath déplie un ~ en chemin absolu.
func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

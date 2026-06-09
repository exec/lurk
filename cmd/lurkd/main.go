// Command lurkd is the Lurk bouncer daemon. It holds persistent connections to
// upstream IRC networks and serves them to attached lurk clients over a single
// TLS IRC listener, so the client can detach and reattach without losing its
// place. It is headless and standard-library only — unlike cmd/lurk it imports no
// charm UI libraries (enforced by server.TestNoCharmDependency).
//
// # Lifecycle
//
// On SIGINT or SIGTERM lurkd performs a graceful shutdown:
//  1. Close the listener — stop accepting new connections.
//  2. Drain a brief window (see shutdownDrainTimeout) so in-flight sessions can
//     finish their current send before the next step.
//  3. mgr.Close() — signal all upstream clients to disconnect and wait for their
//     run goroutines (including any background initial-connect retry goroutines)
//     to exit.
//  4. store.Close() — flush and close all JSONL file handles.
//  5. cursors.Close() — perform a final cursor flush then stop the periodic-flush
//     goroutine.
//
// A second SIGINT/SIGTERM forces immediate exit via os.Exit(1), preventing a hang
// when upstream networks do not respond promptly.
//
// # Flags
//
//	-config <path>     config-file path (env LURKD_CONFIG; default ~/.config/lurkd/config.json)
//	-listen <addr>     listen address, overriding the config (e.g. :6697)
//	-version           print version and exit
//	-hashpw            read a password from stdin, print the PBKDF2 hash, and exit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/server"
)

// version is set at build time via -ldflags "-X main.version=...", matching cmd/lurk.
var version = "dev"

// shutdownDrainTimeout is the brief window lurkd waits after closing the
// listener before tearing down upstreams. It gives in-flight sessions a chance
// to finish their current send/receive cycle cleanly. It is intentionally short
// (1s) — the real drain is the upstream client.Close(), which waits for the run
// goroutine to exit.
const shutdownDrainTimeout = 1 * time.Second

func main() {
	log.SetFlags(0)
	log.SetPrefix("lurkd: ")

	var (
		configPath  = flag.String("config", os.Getenv("LURKD_CONFIG"), "config-file path (env LURKD_CONFIG); default ~/.config/lurkd/config.json")
		listenAddr  = flag.String("listen", "", "listen address, overriding the config (e.g. :6697)")
		showVersion = flag.Bool("version", false, "print version and exit")
		hashPW      = flag.Bool("hashpw", false, "read a password from stdin, print the PBKDF2 hash, and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("lurkd " + version)
		return
	}

	if *hashPW {
		runHashpw()
		return
	}

	cfg, path, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *listenAddr != "" {
		cfg.Listen.Addr = *listenAddr
	}

	log.Printf("loaded %d network(s) from %s", len(cfg.Networks), path)

	addr := cfg.Listen.Addr
	if addr == "" {
		log.Printf("no listen address configured and -listen not given; exiting")
		log.Printf("hint: set listen.addr in %s or pass -listen :6697", path)
		os.Exit(1)
	}

	// Build the backlog store. DefaultPath uses $LURKD_BACKLOG_DIR if set,
	// else the XDG data home. On error, proceed without a store (CHATHISTORY
	// will return empty results, but the daemon is otherwise functional).
	backlogDir, err := backlog.DefaultPath()
	if err != nil {
		log.Printf("backlog: could not determine store path: %v (CHATHISTORY will be unavailable)", err)
	}
	var store *backlog.Store
	if backlogDir != "" {
		// Cap each per-(netid,target) JSONL file so backlog disk use stays
		// bounded on a long-running daemon. One rotation is kept (.jsonl +
		// .jsonl.1), so on-disk history per target is at most ~2× this value.
		const backlogFileCap = 16 << 20 // 16 MiB
		store, err = backlog.NewStore(backlogDir, backlog.WithMaxFileSize(backlogFileCap))
		if err != nil {
			log.Printf("backlog: open store at %s: %v (CHATHISTORY will be unavailable)", backlogDir, err)
		} else {
			log.Printf("backlog store: %s", backlogDir)
		}
	}

	// Build the cursor store under the lurkd data directory.
	cursorsDir := cursorStoreDir(backlogDir)
	var cursors *server.CursorStore
	if cursorsDir != "" {
		cursors, err = server.NewCursorStore(cursorsDir, 0)
		if err != nil {
			log.Printf("cursors: open store at %s: %v (per-client cursors will be unavailable)", cursorsDir, err)
		} else {
			log.Printf("cursor store: %s", cursorsDir)
		}
	}

	// Build the Server first (it becomes the Sink the Manager feeds into).
	srv := server.New(cfg)
	if store != nil {
		srv.WithStore(store)
	}
	if cursors != nil {
		srv.WithCursorStore(cursors)
	}
	srv.WithConfigPath(path)

	// Build the upstream Manager with the Server as its Sink. The Server must
	// be the Sink so that Ingest fans out to bound sessions as well as storing.
	mgr := server.NewManager(cfg, srv)
	srv.WithManager(mgr)

	// Set up signal handling. The first signal triggers graceful shutdown;
	// the second forces immediate exit.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	ctx, cancelShutdown := context.WithCancel(context.Background())
	go func() {
		sig := <-sigCh
		log.Printf("signal: received %s — starting graceful shutdown", sig)
		cancelShutdown()
		// Second signal forces immediate exit.
		sig = <-sigCh
		log.Printf("signal: received %s again — forcing immediate exit", sig)
		os.Exit(1)
	}()

	// Start all configured upstreams. Start is now non-fatal: networks that
	// fail their initial connect are retried in the background.
	if len(cfg.Networks) > 0 {
		startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
		defer startCancel()
		if err := mgr.Start(startCtx); err != nil {
			// Only a configuration-level error (currently unreachable).
			log.Fatalf("start upstreams: %v", err)
		}
		log.Printf("upstreams started (%d configured)", len(cfg.Networks))
	}

	// Determine TLS: if no cert/key is configured, generate a self-signed cert
	// on first run (written to the config directory).
	if cfg.Listen.TLSCert == "" || cfg.Listen.TLSKey == "" {
		certDir := filepath.Dir(path)
		if _, err := server.EnsureSelfSignedCert(certDir, cfg, path); err != nil {
			log.Printf("TLS: could not generate self-signed cert: %v — falling back to plain TCP (AUTHENTICATE PLAIN will be refused)", err)
		}
	}

	// Start the listener.
	ln, err := server.NewListener(addr, cfg)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on %s", ln.Addr())

	// Serve in a goroutine so the main goroutine can handle shutdown.
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ln)
	}()

	// Wait for either the server to fail or a shutdown signal.
	select {
	case err := <-serveDone:
		if err != nil {
			log.Printf("serve error: %v", err)
		}
	case <-ctx.Done():
		// Graceful shutdown sequence.
		log.Printf("shutdown: closing listener")
		_ = ln.Close()

		// Brief drain window for in-flight sessions.
		select {
		case <-serveDone:
		case <-time.After(shutdownDrainTimeout):
		}

		log.Printf("shutdown: closing upstream connections")
		mgr.Close()
		log.Printf("shutdown: upstreams closed")

		if store != nil {
			log.Printf("shutdown: closing backlog store")
			store.Close()
		}

		if cursors != nil {
			log.Printf("shutdown: flushing and closing cursor store")
			cursors.Close()
			log.Printf("shutdown: cursors flushed")
		}

		log.Printf("shutdown: complete")
	}
}

// runHashpw reads a password as a single line from stdin, hashes it with PBKDF2
// via server.HashPassword, and prints the hash string to stdout. This is the
// bootstrap mechanism: an admin runs `lurkd -hashpw`, pastes the result into the
// config, and restarts.
//
// The password is not logged. Note that lurkd is stdlib-only and therefore does
// not disable terminal echo; when stdin is a TTY the input is visible and may be
// retained in scrollback. To avoid that, pipe the password in instead, e.g.
// `printf %s "$pw" | lurkd -hashpw` (still readable from shell history) or feed
// it from a file. We warn on the terminal case below.
func runHashpw() {
	if fi, statErr := os.Stdin.Stat(); statErr == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintln(os.Stderr, "warning: terminal echo is not disabled — the password will be visible and may remain in scrollback.")
		fmt.Fprintln(os.Stderr, "         pipe the password in (e.g. from a file) to avoid this.")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			log.Fatalf("hashpw: read password: %v", err)
		}
		log.Fatalf("hashpw: no password provided")
	}
	pw := scanner.Text()
	if pw == "" {
		log.Fatalf("hashpw: empty password")
	}
	hash, err := server.HashPassword(pw)
	if err != nil {
		log.Fatalf("hashpw: %v", err)
	}
	fmt.Println(hash)
}

// loadConfig loads from an explicit path when one is given (flag or LURKD_CONFIG),
// otherwise from the default resolved path. It returns the config and the path it
// came from.
func loadConfig(explicit string) (*server.Config, string, error) {
	if explicit != "" {
		cfg, err := server.LoadFrom(explicit)
		return cfg, explicit, err
	}
	return server.Load()
}

// cursorStoreDir derives the cursor store directory from the backlog directory.
// The cursor store lives alongside the backlog under the lurkd data dir, as
// $XDG_DATA_HOME/lurkd/cursors (or ~/.local/share/lurkd/cursors). If backlogDir
// is empty (backlog unavailable), we resolve the data dir from scratch via
// the same XDG logic.
func cursorStoreDir(backlogDir string) string {
	if backlogDir != "" {
		// backlogDir is .../lurkd/backlog; cursors go in .../lurkd/cursors.
		return filepath.Join(filepath.Dir(backlogDir), "cursors")
	}
	// Fallback: derive from XDG_DATA_HOME directly.
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "lurkd", "cursors")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "lurkd", "cursors")
}

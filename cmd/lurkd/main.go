// Command lurkd is the Lurk bouncer daemon. It holds persistent connections to
// upstream IRC networks and serves them to attached lurk clients over a single
// TLS IRC listener, so the client can detach and reattach without losing its
// place. It is headless and standard-library only — unlike cmd/lurk it imports no
// charm UI libraries (enforced by server.TestNoCharmDependency).
//
// Phase 6b: the full daemon wiring is in place. Upstream events flow through
// the Manager → Server.Ingest (store + fanout) → bound clients. Bound clients
// relay commands back to the upstream via the Manager. The backlog store is
// wired for CHATHISTORY. Signal handling and graceful shutdown are Phase 9.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/server"
)

// version is set at build time via -ldflags "-X main.version=...", matching cmd/lurk.
var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("lurkd: ")

	var (
		configPath  = flag.String("config", os.Getenv("LURKD_CONFIG"), "config-file path (env LURKD_CONFIG); default ~/.config/lurkd/config.json")
		listenAddr  = flag.String("listen", "", "listen address, overriding the config (e.g. :6667)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("lurkd " + version)
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
		log.Printf("hint: set listen.addr in %s or pass -listen :6667", path)
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
		store, err = backlog.NewStore(backlogDir)
		if err != nil {
			log.Printf("backlog: open store at %s: %v (CHATHISTORY will be unavailable)", backlogDir, err)
		} else {
			log.Printf("backlog store: %s", backlogDir)
			defer store.Close()
		}
	}

	// Build the Server first (it becomes the Sink the Manager feeds into).
	srv := server.New(cfg)
	if store != nil {
		srv.WithStore(store)
	}
	srv.WithConfigPath(path)

	// Build the upstream Manager with the Server as its Sink. The Server must
	// be the Sink so that Ingest fans out to bound sessions as well as storing.
	mgr := server.NewManager(cfg, srv)
	srv.WithManager(mgr)

	// Start all configured upstreams. Use a background context for the dial
	// phase; full lifecycle management (signal-driven shutdown) is Phase 9.
	startCtx := context.Background()
	if len(cfg.Networks) > 0 {
		if err := mgr.Start(startCtx); err != nil {
			log.Fatalf("start upstreams: %v", err)
		}
		log.Printf("upstreams started")
	}

	// Start the listener.
	ln, err := server.NewListener(addr, cfg)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on %s", ln.Addr())

	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
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

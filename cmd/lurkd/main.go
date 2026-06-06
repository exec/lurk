// Command lurkd is the Lurk bouncer daemon. It holds persistent connections to
// upstream IRC networks and serves them to attached lurk clients over a single
// TLS IRC listener, so the client can detach and reattach without losing its
// place. It is headless and standard-library only — unlike cmd/lurk it imports no
// charm UI libraries (enforced by server.TestNoCharmDependency).
//
// This is the Phase 0 skeleton: flag parsing and config load only. The TLS
// listener, the server-side registration/CAP/SASL responder, the upstream session
// manager, the backlog store, and the soju.im/bouncer-networks surface land in
// later phases (see docs/LURKD-DESIGN.md).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/exec/lurk/server"
)

// version is set at build time via -ldflags "-X main.version=...", matching cmd/lurk.
var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("lurkd: ")

	var (
		configPath  = flag.String("config", os.Getenv("LURKD_CONFIG"), "config-file path (env LURKD_CONFIG); default ~/.config/lurkd/config.json")
		listenAddr  = flag.String("listen", "", "TLS listen address, overriding the config (e.g. :6697)")
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
	log.Printf("listen address %q", cfg.Listen.Addr)
	// Phase 0 skeleton: the listener and session multiplexer are not wired up yet.
	log.Printf("the TLS listener is not implemented yet (Phase 1) — nothing to serve, exiting")
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

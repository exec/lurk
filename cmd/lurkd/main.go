// Command lurkd is the Lurk bouncer daemon. It holds persistent connections to
// upstream IRC networks and serves them to attached lurk clients over a single
// TLS IRC listener, so the client can detach and reattach without losing its
// place. It is headless and standard-library only — unlike cmd/lurk it imports no
// charm UI libraries (enforced by server.TestNoCharmDependency).
//
// Phase 1: the plain TCP accept loop is wired. TLS wrapping, SASL auth, session
// multiplexing, backlog store, and soju.im/bouncer-networks land in later phases
// (see docs/LURKD-DESIGN.md §8).
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

	ln, err := server.NewListener(addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on %s (plain TCP; TLS enforcement is Phase 2)", ln.Addr())

	s := server.New(cfg)
	if err := s.Serve(ln); err != nil {
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

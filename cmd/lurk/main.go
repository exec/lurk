// Command lurk is a small smoke-test client for the lurk IRC library: it
// connects to a server, negotiates capabilities (and optionally authenticates
// with SASL), joins a channel, prints the messages it sees, and reads lines
// from stdin so the user can chat interactively.
//
// Configuration comes from flags, each of which falls back to an environment
// variable (LURK_SERVER, LURK_NICK, ...) so it is convenient to run from a
// shell or a container without leaking secrets onto the command line.
//
// Example:
//
//	lurk -server irc.libera.chat:6697 -tls -nick lurkbot -channel '#lurk-test'
//	LURK_SASL_PASS=secret lurk -server ... -sasl PLAIN -sasl-user lurkbot ...
//
// Interactive input (stdin):
//
//	A plain line is sent as a PRIVMSG to the current channel (the -channel arg,
//	updated by /join). Lines beginning with '/' are commands: /join, /part,
//	/msg, /me, /away, /nick, /names, /list, /quit, /raw. A line starting with '//' sends
//	a literal message that begins with a single '/'. Ctrl-D (EOF) or Ctrl-C quits
//	cleanly.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/config"
	"github.com/exec/lurk/irc"
	"github.com/exec/lurk/tui"
)

// version is the build version, overridden at release time via
// -ldflags "-X main.version=<v>". It defaults to "dev" for local builds.
var version = "dev"

func main() {
	log.SetFlags(log.Ltime)

	cfg, channel, plain, configPath, logDir := parseConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load the saved-network store: it drives the launcher and the runtime
	// /connect command. A missing file yields an empty store (not an error).
	store, storePath, err := loadStore(configPath)
	if err != nil {
		log.Fatalf("lurk: %v", err)
	}

	// With no -server, open the launcher: pick (or create) a saved network and
	// build the connection config from it. The launcher is a self-contained TUI
	// that runs to completion before we connect.
	if cfg.Server == "" {
		net, defaults, err := resolveNetwork(ctx, store, storePath)
		if err != nil {
			log.Fatalf("lurk: %v", err)
		}
		if net == nil {
			return // user quit the launcher without choosing
		}
		var channels []string
		cfg, channels = networkToConfig(*net, defaults)
		channel = strings.Join(channels, ",")
	}

	if cfg.Nick == "" {
		log.Fatal("lurk: a nickname is required (-nick or LURK_NICK)")
	}
	if cfg.Server == "" {
		log.Fatal("lurk: a server is required (-server or LURK_SERVER)")
	}

	c := client.New(cfg)

	regCtx, regCancel := context.WithTimeout(ctx, 30*time.Second)
	defer regCancel()

	if plain {
		runPlain(ctx, cancel, regCtx, c, channel)
		return
	}
	runTUI(ctx, regCtx, c, channel, logDir, makeConnectFunc(ctx, store))
}

// runTUI connects the client and hands control to the Bubble Tea interface. The
// TUI consumes the client's event stream (no print handlers are registered);
// tui.Run owns the program lifecycle and returns when the user quits or the
// connection ends, after which we tear the client down cleanly.
func runTUI(ctx, regCtx context.Context, c *client.Client, channel, logDir string, connect tui.ConnectFunc) {
	if err := c.Connect(regCtx); err != nil {
		log.Fatalf("lurk: connect: %v", err)
	}
	if channel != "" {
		// Joining before the program starts lets the initial JOIN/NAMES events
		// flow through the buffered event stream and open the buffer on screen.
		if err := c.Join(channel); err != nil {
			log.Printf("join %s: %v", channel, err)
		}
	}

	err := tui.Run(ctx, c, logDir, connect)

	// tui.Run has restored the primary screen by now; tear down the connection.
	_ = c.Quit("lurk signing off")
	_ = c.Close()
	if err != nil {
		log.Printf("lurk: %v", err)
	}
}

// runPlain is the original line-mode client: print handlers + a stdin REPL. It
// is selected with -plain (or LURK_PLAIN) for scripting and for terminals where
// the full TUI is undesirable.
func runPlain(ctx context.Context, cancel context.CancelFunc, regCtx context.Context, c *client.Client, channel string) {
	registerHandlers(c, channel)

	if err := c.Connect(regCtx); err != nil {
		log.Fatalf("lurk: connect: %v", err)
	}
	log.Printf("registered as %s on %s", c.Nick(), orUnknown(c.Network()))

	ui := &repl{client: c, current: channel}
	if channel != "" {
		if err := c.Join(channel); err != nil {
			log.Printf("join %s: %v", channel, err)
		}
	}

	// Read user input from stdin in the background. EOF (Ctrl-D) cancels the
	// signal context so the shutdown path below runs exactly once, the same as
	// SIGINT. The loop does not touch c.Wait(), so it can never block shutdown.
	go func() {
		ui.run(os.Stdin)
		cancel() // stdin closed: trigger the same clean teardown as a signal
	}()

	// Tear the connection down cleanly when the signal context is cancelled
	// (SIGINT/SIGTERM or stdin EOF). Quit, give it a moment to flush, then close.
	go func() {
		<-ctx.Done()
		_ = c.Quit("lurk signing off")
		time.Sleep(200 * time.Millisecond)
		_ = c.Close()
	}()

	if err := c.Wait(); err != nil {
		log.Printf("disconnected: %v", err)
	} else {
		log.Print("disconnected")
	}
}

// repl is the interactive stdin input loop. It tracks a "current channel" (the
// default target for plain message lines), which /join updates. It is safe for
// the input goroutine to mutate current while handlers read it via Current.
type repl struct {
	client *client.Client

	mu      sync.Mutex
	current string
}

// Current returns the active default target channel.
func (r *repl) Current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// setCurrent updates the active default target channel.
func (r *repl) setCurrent(ch string) {
	r.mu.Lock()
	r.current = ch
	r.mu.Unlock()
}

// run reads lines from in until EOF, dispatching each to a slash-command
// handler or sending it as a message to the current channel. It returns when
// the input stream closes (Ctrl-D) or errors.
func (r *repl) run(in *os.File) {
	sc := bufio.NewScanner(in)
	// Allow long lines (pasted text); IRC will still bound what actually sends.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		// "//..." is a literal message starting with a single '/'.
		if strings.HasPrefix(line, "//") {
			r.sendMessage(line[1:])
			continue
		}
		if strings.HasPrefix(line, "/") {
			r.command(line)
			continue
		}
		r.sendMessage(line)
	}
}

// sendMessage sends text to the current channel as a PRIVMSG. When the server's
// echo-message capability is NOT enabled, it locally echoes the line so the user
// sees their own message (with echo-message, the server echoes it back through
// the HandleMessage printer instead, so we stay quiet to avoid a double print).
func (r *repl) sendMessage(text string) {
	target := r.Current()
	if target == "" {
		fmt.Println("*** no current channel; use /join <#chan> or /msg <target> <text>")
		return
	}
	if err := r.client.Privmsg(target, text); err != nil {
		fmt.Printf("*** send failed: %v\n", err)
		return
	}
	if !r.client.CapEnabled("echo-message") {
		// Sanitize the echo too: pasted text may carry escape sequences.
		fmt.Printf("<%s/%s> %s\n", client.SanitizeTerminal(target), r.client.Nick(), client.SanitizeTerminal(text))
	}
}

// command parses and dispatches a slash command line.
func (r *repl) command(line string) {
	// Split into the command word and the remainder (args), preserving spaces in
	// the remainder for message bodies.
	cmd, rest, _ := strings.Cut(line[1:], " ")
	rest = strings.TrimSpace(rest)

	switch strings.ToLower(cmd) {
	case "join", "j":
		if rest == "" {
			fmt.Println("*** usage: /join <#channel>")
			return
		}
		ch := strings.Fields(rest)[0]
		if err := r.client.Join(ch); err != nil {
			fmt.Printf("*** join failed: %v\n", err)
			return
		}
		r.setCurrent(ch)
		fmt.Printf("*** current channel is now %s\n", ch)

	case "part", "leave":
		ch := rest
		if ch == "" {
			ch = r.Current()
		}
		if ch == "" {
			fmt.Println("*** usage: /part [<#channel>]")
			return
		}
		if err := r.client.Part(ch); err != nil {
			fmt.Printf("*** part failed: %v\n", err)
			return
		}
		if ch == r.Current() {
			r.setCurrent("")
		}

	case "msg", "m":
		target, body, ok := strings.Cut(rest, " ")
		if !ok || strings.TrimSpace(body) == "" {
			fmt.Println("*** usage: /msg <target> <text>")
			return
		}
		if err := r.client.Privmsg(target, body); err != nil {
			fmt.Printf("*** msg failed: %v\n", err)
			return
		}
		if !r.client.CapEnabled("echo-message") {
			fmt.Printf("<%s/%s> %s\n", client.SanitizeTerminal(target), r.client.Nick(), client.SanitizeTerminal(body))
		}

	case "me":
		target := r.Current()
		if target == "" {
			fmt.Println("*** no current channel; use /join <#chan> or /msg <target> <text>")
			return
		}
		if rest == "" {
			fmt.Println("*** usage: /me <action text>")
			return
		}
		if err := r.client.Action(target, rest); err != nil {
			fmt.Printf("*** me failed: %v\n", err)
			return
		}
		if !r.client.CapEnabled("echo-message") {
			fmt.Printf("* %s/%s %s\n", client.SanitizeTerminal(target), r.client.Nick(), client.SanitizeTerminal(rest))
		}

	case "away":
		if err := r.client.Away(rest); err != nil {
			fmt.Printf("*** away failed: %v\n", err)
			return
		}
		if rest == "" {
			fmt.Println("*** marked back")
		} else {
			fmt.Printf("*** marked away: %s\n", client.SanitizeTerminal(rest))
		}

	case "nick":
		if rest == "" {
			fmt.Println("*** usage: /nick <newnick>")
			return
		}
		if err := r.client.SetNick(strings.Fields(rest)[0]); err != nil {
			fmt.Printf("*** nick failed: %v\n", err)
		}

	case "names":
		ch := rest
		if ch == "" {
			ch = r.Current()
		}
		if ch == "" {
			fmt.Println("*** usage: /names [<#channel>]")
			return
		}
		if err := r.client.Names(ch); err != nil {
			fmt.Printf("*** names failed: %v\n", err)
		}

	case "list":
		var err error
		if rest == "" {
			err = r.client.List()
		} else {
			err = r.client.List(strings.Fields(rest)[0])
		}
		if err != nil {
			fmt.Printf("*** list failed: %v\n", err)
		}

	case "raw":
		if rest == "" {
			fmt.Println("*** usage: /raw <protocol line>")
			return
		}
		if err := r.client.SendRaw(rest); err != nil {
			fmt.Printf("*** raw failed: %v\n", err)
		}

	case "quit", "q":
		// Send QUIT; the run loop will end and main's c.Wait() returns.
		_ = r.client.Quit(rest)

	default:
		fmt.Printf("*** unknown command /%s — try: /join /part /msg /me /away /nick /names /list /quit /raw (// for a literal /message)\n", client.SanitizeTerminal(cmd))
	}
}

// parseConfig builds a client.Config from flags backed by environment
// variables, and returns the channel to join, whether to use the plain client,
// and the config-file path override (-config / LURK_CONFIG, "" for the default).
func parseConfig() (cfg client.Config, channel string, plain bool, configPath, logDir string) {
	var (
		server     = flag.String("server", env("LURK_SERVER", ""), "server host:port (env LURK_SERVER)")
		nick       = flag.String("nick", env("LURK_NICK", "lurk"), "nickname (env LURK_NICK)")
		user       = flag.String("user", env("LURK_USER", ""), "username/ident, defaults to nick (env LURK_USER)")
		realname   = flag.String("realname", env("LURK_REALNAME", "lurk IRC client"), "realname (env LURK_REALNAME)")
		pass       = flag.String("pass", env("LURK_PASS", ""), "server password (env LURK_PASS)")
		useTLS     = flag.Bool("tls", envBool("LURK_TLS", false), "connect with TLS (env LURK_TLS)")
		insecure   = flag.Bool("insecure", envBool("LURK_INSECURE", false), "skip TLS certificate verification (env LURK_INSECURE)")
		channelArg = flag.String("channel", env("LURK_CHANNEL", ""), "channel to join (env LURK_CHANNEL)")
		plainArg   = flag.Bool("plain", envBool("LURK_PLAIN", false), "use the plain line-mode client instead of the full-screen TUI (env LURK_PLAIN)")
		configArg  = flag.String("config", env("LURK_CONFIG", ""), "config-file path for the network launcher (env LURK_CONFIG)")
		logArg     = flag.Bool("log", envBool("LURK_LOG", false), "write per-channel chat logs to disk (env LURK_LOG)")

		saslMech    = flag.String("sasl", env("LURK_SASL", ""), "SASL mechanism: PLAIN or EXTERNAL (env LURK_SASL)")
		saslUser    = flag.String("sasl-user", env("LURK_SASL_USER", ""), "SASL username (env LURK_SASL_USER)")
		saslPass    = flag.String("sasl-pass", env("LURK_SASL_PASS", ""), "SASL password (env LURK_SASL_PASS)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("lurk", version)
		os.Exit(0)
	}

	cfg = client.Config{
		Nick:               *nick,
		User:               *user,
		Realname:           *realname,
		Pass:               *pass,
		Server:             *server,
		TLS:                *useTLS,
		InsecureSkipVerify: *insecure,
		AutoReconnect:      true,
		Version:            "lurk " + version,
		SASL: client.SASLConfig{
			Mechanism: strings.ToUpper(*saslMech),
			Username:  *saslUser,
			Password:  *saslPass,
		},
		// Request the library defaults plus echo-message: in an interactive UI
		// it lets the server confirm our own PRIVMSGs (printed via HandleMessage),
		// so the REPL suppresses its local echo when it is negotiated.
		Caps: append(append([]string(nil), client.DefaultCaps...), "echo-message"),
	}

	if *logArg {
		dir, err := config.LogDir()
		if err != nil {
			log.Fatalf("lurk: resolve log directory: %v", err)
		}
		logDir = dir
	}
	return cfg, *channelArg, *plainArg, *configArg, logDir
}

// networkToConfig maps a saved network to a client.Config (falling back to def
// for blank identity fields, and to "lurk" for a still-blank nick) and returns
// its autojoin channels.
func networkToConfig(n config.Network, def config.Identity) (client.Config, []string) {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	cfg := client.Config{
		Nick:               pick(n.Nick, def.Nick, "lurk"),
		User:               pick(n.User, def.User),
		Realname:           pick(n.Realname, def.Realname, "lurk IRC client"),
		Pass:               n.Pass,
		Server:             n.Addr,
		TLS:                n.TLS,
		InsecureSkipVerify: n.Insecure,
		AllowInsecureAuth:  n.AllowInsecureAuth,
		AutoReconnect:      true,
		Version:            "lurk " + version,
		Highlights:         n.Highlights,
		SASL: client.SASLConfig{
			Mechanism: strings.ToUpper(n.SASL.Mechanism),
			Username:  n.SASL.Username,
			Password:  n.SASL.Password,
		},
		Caps: append(append([]string(nil), client.DefaultCaps...), "echo-message"),
	}
	return cfg, n.Channels
}

// resolveNetwork loads the launcher config and resolves the network to connect
// to: a positional `lurk <name>` argument connects directly to the saved network
// of that name, otherwise the interactive launcher runs. A nil network with no
// error means the user quit the launcher without choosing.
func resolveNetwork(ctx context.Context, store *config.File, path string) (*config.Network, config.Identity, error) {
	if args := flag.Args(); len(args) > 0 {
		name := args[0]
		n, ok := store.Get(name)
		if !ok {
			return nil, config.Identity{}, fmt.Errorf("no saved network named %q (run lurk with no arguments to add one)", name)
		}
		return &n, store.Defaults, nil
	}

	net, err := tui.Launch(ctx, store, path)
	if err != nil {
		return nil, config.Identity{}, err
	}
	return net, store.Defaults, nil
}

// loadStore loads the saved-network store from configPath (or the default
// location), returning it plus the resolved path.
func loadStore(configPath string) (*config.File, string, error) {
	if configPath != "" {
		s, err := config.LoadFrom(configPath)
		return s, configPath, err
	}
	return config.Load()
}

// makeConnectFunc returns the callback the TUI's /connect command uses to dial an
// additional saved network at runtime. It resolves the name against store, dials
// (with a fresh registration timeout derived from ctx), and requests autojoins.
func makeConnectFunc(ctx context.Context, store *config.File) tui.ConnectFunc {
	return func(name string) (*client.Client, error) {
		n, ok := store.Get(name)
		if !ok {
			return nil, fmt.Errorf("no saved network named %q", name)
		}
		cfg, channels := networkToConfig(n, store.Defaults)
		c := client.New(cfg)
		rc, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := c.Connect(rc); err != nil {
			return nil, err
		}
		for _, ch := range channels {
			if ch != "" {
				_ = c.Join(ch)
			}
		}
		return c, nil
	}
}

// registerHandlers wires the printing handlers used by the smoke test.
//
// Every interpolated field below is server- or peer-controlled (nicks, message
// bodies, channel names, reasons), and these handlers write straight to the
// terminal — so each such field is run through client.SanitizeTerminal first.
// Without it a hostile peer could embed ANSI/OSC escape sequences in, say, a
// PRIVMSG or a part reason and drive the user's terminal (clipboard, title,
// cursor-forged output). The TUI front-end sanitizes the same way; the format
// strings themselves are constant, so only the arguments need wrapping.
func registerHandlers(c *client.Client, channel string) {
	san := client.SanitizeTerminal

	c.HandleConnected(func(ev *client.Event) {
		fmt.Printf("*** connected (welcome: %s)\n", san(ev.Text()))
	})

	c.HandleReconnecting(func(ev *client.Event) { fmt.Printf("*** %s\n", san(ev.Text())) })
	c.HandleReconnected(func(ev *client.Event) { fmt.Printf("*** %s\n", san(ev.Text())) })

	c.HandleMessage(func(ev *client.Event) {
		target := ev.Param(0)
		if target == c.Nick() {
			// A private message to us; show the sender as the context.
			target = ev.Nick()
		}
		// A CTCP ACTION (/me) renders as "* target/nick text" rather than a normal
		// "<target/nick> body" line; other CTCP types fall through to the latter.
		if act, ok := client.CTCPAction(ev.Text()); ok {
			fmt.Printf("* %s/%s %s\n", san(target), san(ev.Nick()), san(act))
			return
		}
		fmt.Printf("<%s/%s> %s\n", san(target), san(ev.Nick()), san(ev.Text()))
	})

	c.HandleJoin(func(ev *client.Event) {
		fmt.Printf("--> %s joined %s\n", san(ev.Nick()), san(ev.Param(0)))
	})
	c.HandlePart(func(ev *client.Event) {
		fmt.Printf("<-- %s left %s (%s)\n", san(ev.Nick()), san(ev.Param(0)), san(ev.Text()))
	})
	c.HandleQuit(func(ev *client.Event) {
		fmt.Printf("<-- %s quit (%s)\n", san(ev.Nick()), san(ev.Text()))
	})
	c.HandleNick(func(ev *client.Event) {
		fmt.Printf("*** %s is now known as %s\n", san(ev.Nick()), san(ev.Param(0)))
	})

	c.On(irc.NOTICE, func(ev *client.Event) {
		fmt.Printf("-%s- %s\n", san(ev.Nick()), san(ev.Text()))
	})

	// Standard replies (FAIL/WARN/NOTE): "<TYPE> <COMMAND> <code> [ctx] :<desc>".
	stdReply := func(ev *client.Event) {
		ctx := ""
		if n := len(ev.Message.Params); n > 3 {
			ctx = " " + san(strings.Join(ev.Message.Params[2:n-1], " "))
		}
		fmt.Printf("! %s %s %s%s: %s\n", ev.Command(), san(ev.Param(0)), san(ev.Param(1)), ctx, san(ev.Text()))
	}
	c.On(irc.FAIL, stdReply)
	c.On(irc.WARN, stdReply)
	c.On(irc.NOTE, stdReply)

	// Channel directory (/list): one line per channel, then a terminator.
	c.On(irc.RPL_LIST, func(ev *client.Event) {
		fmt.Printf("*** %s (%s users) %s\n", san(ev.Param(1)), san(ev.Param(2)), san(ev.Text()))
	})
	c.On(irc.RPL_LISTEND, func(ev *client.Event) {
		fmt.Println("*** end of channel list")
	})

	// Print the member list once NAMES completes for our channel.
	c.On(irc.RPL_ENDOFNAMES, func(ev *client.Event) {
		ch := ev.Param(1)
		members := c.Members(ch)
		names := make([]string, 0, len(members))
		for _, m := range members {
			names = append(names, san(m.Prefixes+m.Nick))
		}
		fmt.Printf("*** %s members (%d): %s\n", san(ch), len(names), strings.Join(names, " "))
	})
}

// env returns the value of key, or def if unset/empty.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envBool reports a boolean environment variable, treating "1", "true", "yes",
// and "on" (case-insensitive) as true.
func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// orUnknown returns s, or "(unknown network)" when empty.
func orUnknown(s string) string {
	if s == "" {
		return "(unknown network)"
	}
	return s
}

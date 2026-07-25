package server_test

import (
	"os/exec"
	"strings"
	"testing"
)

// stdlibOnlyPackages are every package CLAUDE.md mandates stay standard-library
// only: the protocol packages, the config/chatlog helpers, and the whole lurkd
// daemon side. None of them — transitively — may import anything outside the
// standard library, which in particular keeps the charm UI libraries confined to
// tui/ and cmd/lurk. Add each new stdlib-only package here as it lands.
var stdlibOnlyPackages = []string{
	"github.com/exec/lurk/irc",
	"github.com/exec/lurk/conn",
	"github.com/exec/lurk/cap",
	"github.com/exec/lurk/sasl",
	"github.com/exec/lurk/isupport",
	"github.com/exec/lurk/client",
	"github.com/exec/lurk/config",
	"github.com/exec/lurk/chatlog",
	"github.com/exec/lurk/server",
	"github.com/exec/lurk/bouncer",
	"github.com/exec/lurk/backlog",
	"github.com/exec/lurk/cmd/lurkd",
}

// TestStdlibOnlyDependencies enforces the dependency rule by asking the
// toolchain for each package's full transitive import closure and asserting
// every entry is either the standard library or another lurk package. Any
// third-party module — charm or otherwise — shows up as a non-standard,
// dot-bearing import path and fails the test.
//
// It shells out to `go list -deps` (the toolchain is present whenever the tests
// run); if go is somehow unavailable the check skips rather than failing
// spuriously.
func TestStdlibOnlyDependencies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping dependency guard")
	}
	const modulePrefix = "github.com/exec/lurk/"
	for _, pkg := range stdlibOnlyPackages {
		t.Run(pkg, func(t *testing.T) {
			// {{.Standard}} is the toolchain's own answer to "is this in
			// GOROOT?", which correctly accounts for the stdlib's vendored
			// copies (e.g. vendor/golang.org/x/net/dns/dnsmessage) that carry
			// dots in their paths but are still standard library.
			out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", pkg).CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				dep, std, ok := strings.Cut(line, " ")
				if !ok {
					t.Fatalf("unexpected `go list` output line for %s: %q", pkg, line)
				}
				if std == "true" {
					continue
				}
				if strings.HasPrefix(dep, modulePrefix) || dep == strings.TrimSuffix(modulePrefix, "/") {
					continue // our own packages, themselves covered by this guard
				}
				// Everything left is a third-party module. Name charm
				// explicitly since that is the rule most likely to be broken by
				// accident.
				if strings.Contains(dep, "charm.land/") || strings.Contains(dep, "charmbracelet/") {
					t.Errorf("%s transitively imports a charm library (%s) — charm imports are confined to tui/ and cmd/lurk", pkg, dep)
					continue
				}
				t.Errorf("%s transitively imports non-stdlib package %s — this package must stay standard-library only", pkg, dep)
			}
		})
	}
}

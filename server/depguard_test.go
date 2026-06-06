package server_test

import (
	"os/exec"
	"strings"
	"testing"
)

// lurkdPackages are the daemon packages that must stay standard-library only.
// lurkd is headless, so none of them — transitively — may import the charm UI
// libraries that CLAUDE.md restricts to tui/ and cmd/lurk. Add each new daemon
// package here as it lands.
var lurkdPackages = []string{
	"github.com/exec/lurk/server",
	"github.com/exec/lurk/cmd/lurkd",
	// "github.com/exec/lurk/bouncer",  // Phase 6
	// "github.com/exec/lurk/backlog",  // Phase 4
}

// TestNoCharmDependency enforces the dependency rule for the bouncer daemon by
// asking the toolchain for each package's full transitive import closure and
// asserting no charm library appears. It shells out to `go list -deps` (the
// toolchain is present whenever the tests run); if go is somehow unavailable the
// check skips rather than failing spuriously.
func TestNoCharmDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping dependency guard")
	}
	for _, pkg := range lurkdPackages {
		t.Run(pkg, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
			}
			for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.Contains(dep, "charm.land/") {
					t.Errorf("%s transitively imports a charm library (%s) — lurkd must stay stdlib-only", pkg, dep)
				}
			}
		})
	}
}

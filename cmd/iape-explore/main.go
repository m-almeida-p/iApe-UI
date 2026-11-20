// Command iape-explore drives the real Web UI through the paths of the P2P flows, with no model in the loop, and says whether each
// one ends where it should. After every step it records what the page shows: the accessibility tree, a screenshot, the requests the
// page made and the lines that are new. It is how the Web UI is accepted against the agent's service, and a way to see how the page
// behaves before writing a test for it.
//
//	iape-explore -list                      the paths
//	iape-explore -path pay                  one path
//	iape-explore -all                       every path, one after the other
//	iape-explore -path pay -amount 35 -contact "Maria Torres"
//	iape-explore "Hola" "expect:Hola"       steps of your own (see internal/explore)
//
// Chrome has a window, to watch the run; -headless runs it without one. Either way a run records a screenshot after each step.
// It signs in as EXPLORE_USER_EMAIL and EXPLORE_USER_PASSWORD, or as the first user of the Web UI (ADMIN_EMAIL and ADMIN_PASSWORD),
// and reads EXPLORE_BASE_URL, EXPLORE_MCP_COMMAND and EXPLORE_MCP_ARGS from .env (see settings.go). Nothing it records holds the
// password or a token.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"iape-ui/internal/platform/envcfg"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	d := deps{
		stdout: os.Stdout,
		stderr: os.Stderr,
		loadSettings: func(envFile string) (settings, error) {
			lookup, err := envcfg.EnvLookup(envFile)
			if err != nil {
				return settings{}, err
			}
			return loadSettings(lookup)
		},
		open: openSession,
		now:  time.Now,
	}
	code := d.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

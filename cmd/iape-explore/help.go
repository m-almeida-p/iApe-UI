package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"iape-ui/internal/explore"
)

// newFlagSet declares the flags of the command into o. Its output goes nowhere: the command says what is wrong and where the help is.
func newFlagSet(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("iape-explore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.envFile, "env", defaultEnvFile, "the settings file")
	fs.StringVar(&o.pathID, "path", "", "run one path of the catalog (see -list)")
	fs.BoolVar(&o.all, "all", false, "run every path of the catalog, each in a conversation of its own (but guard: see -with-guard)")
	fs.BoolVar(&o.list, "list", false, "list the paths and exit")
	fs.StringVar(&o.out, "out", "", "the directory for the records (default temp/iape-explore/<time>)")
	fs.StringVar(&o.contact, "contact", "", `the contact a payment picks, such as "Maria Torres" (default the first; the contact path makes up a new name)`)
	fs.StringVar(&o.amount, "amount", "", "the amount in soles of a payment (default the path's)")
	fs.BoolVar(&o.withGuard, "with-guard", false, "with -all: also run guard, which trips the agent's prompt guard and can get the account banned")
	fs.BoolVar(&o.headless, "headless", false, "run Chrome without a window (the default is a window, to watch the run)")
	fs.DurationVar(&o.settle, "wait", 90*time.Second, "how long a step waits for the page to settle")
	return fs
}

// helpText is what -h says: what the command does, its flags, the paths, the steps, the settings, the records and the exit status.
func helpText() string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("iape-explore drives the Web UI in Chrome through the paths of the P2P flows, with no model in the loop, and says whether each")
	p("one ends where it should. After every step it records what the page shows.")
	p("")
	p("Usage:")
	p("  iape-explore -list                     list the paths")
	p("  iape-explore -path ID [flags]          run one path")
	p("  iape-explore -all [flags]              run every path, each in a conversation of its own")
	p("  iape-explore [flags] STEP...           run steps of your own (see Steps); flags go before the steps, and after -- a step may start with -")
	p("")
	p("Flags:")
	fs := newFlagSet(&options{})
	fs.VisitAll(func(f *flag.Flag) {
		name := "-" + f.Name
		switch f.DefValue {
		case "", "false":
		default:
			name += " (default " + f.DefValue + ")"
		}
		p("  %-26s %s", name, f.Usage)
	})
	p("  -h, -help                  this help")
	p("")
	p("Paths:")
	for _, path := range explore.Paths() {
		note := ""
		if path.Burns {
			note = "   [counts against the account: it can be banned; see -with-guard]"
		}
		p("  %-8s %s%s", path.ID, path.Title, note)
	}
	p("")
	p("Steps (a path is a list of them; a line that is no step below is a message to the agent):")
	for _, f := range explore.StepForms {
		p("  %-28s %s", f.Form, f.Does)
	}
	p("  Cards: approval is pending, otp_required, paid or rejected; contact is pending or confirmed.")
	p("  The first step that fails ends the run, after what the page showed then is recorded.")
	p("")
	p("Settings (in .env, or in the environment, which wins):")
	p("  EXPLORE_BASE_URL                       the Web UI (default http://localhost:8300)")
	p("  EXPLORE_USER_EMAIL, EXPLORE_USER_PASSWORD   who signs in; without them, ADMIN_EMAIL and ADMIN_PASSWORD (the first user)")
	p("  EXPLORE_MCP_COMMAND, EXPLORE_MCP_ARGS  how Chrome DevTools MCP is started: npx on a pinned package, and its flags;")
	p("                                         --isolated and the flags that keep usage statistics off are always required")
	p("")
	p("Records (under -out, one directory for each path):")
	p("  NN-<step>.snapshot.txt, .png, .network.txt    the page's accessibility tree, a screenshot and its requests after step NN")
	p("  report.md                                     the steps, how each went, and what was new on the page")
	p("  summary.md (in -out)                          one line for each path")
	p("  Nothing recorded holds the password or a token.")
	p("")
	p("Exit status: 0 every path ended where it should; 1 the call or the settings are wrong; 2 a path failed; 4 Chrome could not")
	p("be opened or signed in.")
	p("")
	p("Examples:")
	p("  iape-explore -all                                   every path but guard, with a window")
	p("  iape-explore -path pay -amount 35 -contact \"Maria Torres\"")
	p("  iape-explore -headless -all -out /tmp/run           no window, records in /tmp/run")
	p("  iape-explore \"Hola\" \"expect:Hola\" \"expect-no-card\"   steps of your own")
	p("")
	p("Mind the guardrail: guard sends a prompt injection. The agent service counts it against the account, and with a development .env")
	p("two of them in a few minutes can ban the account for a month (see docs/agent-integration-notes.md).")
	return b.String()
}

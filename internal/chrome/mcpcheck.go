package chrome

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var pinned = regexp.MustCompile(`^chrome-devtools-mcp@\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)

// privacyRequired are the flags that keep Chrome DevTools MCP from sending usage statistics and from touching a real browser
// profile. --headless is not one of them: a run with a window is the usual run, and its profile is still a fresh one (--isolated).
var privacyRequired = []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}

// npxFlags are the flags npx may be given before the package, none of which changes which package it runs.
var npxFlags = []string{"-y", "--yes", "--no-install", "--prefer-offline", "-q", "--quiet"}

// DefaultCommand and DefaultArgs start the pinned Chrome DevTools MCP, with a window.
var (
	DefaultCommand = []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}
	DefaultArgs    = []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}
)

// pinnedPackage reports whether the command is npx, run on the pinned package: the package is the first thing after npx and its
// harmless flags. A check that found the pinned name anywhere in the command would pass "npx -y other-package
// chrome-devtools-mcp@1.10.1", or a shell script that mentions it.
func pinnedPackage(command []string) bool {
	if len(command) < 2 {
		return false
	}
	if strings.TrimSuffix(filepath.Base(command[0]), ".cmd") != "npx" {
		return false
	}
	rest := command[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if !slices.Contains(npxFlags, rest[0]) {
			return false
		}
		rest = rest[1:]
	}
	// The package is the last word: what follows it would be an argument of the server, and the arguments are checked on their own.
	return len(rest) == 1 && pinned.MatchString(rest[0])
}

// CheckMCP verifies how Chrome DevTools MCP will be started: npx on a pinned version, so a new release cannot change the
// explorer's behavior, and the flags that keep it from sending usage statistics or touching a real browser profile. A headless run
// also needs --headless; a headed one is the only run that may go without it. A flag that is there and then undone (--isolated=false,
// --no-isolated, --usage-statistics, "--isolated false") is refused too: the server takes the last word.
func CheckMCP(command, args []string, headed bool) error {
	var problems []error
	if !pinnedPackage(command) {
		problems = append(problems, fmt.Errorf(
			"the command must pin a version of chrome-devtools-mcp, like %q: npx, then no flag but %s, then the pinned package (got %q)",
			strings.Join(DefaultCommand, " "), strings.Join(npxFlags, " "), strings.Join(command, " ")))
	}
	required := privacyRequired
	if !headed {
		required = append([]string{"--headless"}, required...)
	}
	var missing []string
	for _, flag := range required {
		if !slices.Contains(args, flag) {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Errorf("the arguments must keep %s (missing: %s)", strings.Join(required, " "), strings.Join(missing, " ")))
	}
	if undone := undoing(args, required); len(undone) > 0 {
		problems = append(problems, fmt.Errorf("the arguments must not undo %s (found: %s)", strings.Join(required, " "), strings.Join(undone, " ")))
	}
	return errors.Join(problems...)
}

// flagKey is the name a flag goes by whatever its spelling: the server reads --usage-statistics, --usageStatistics and
// --no-usage-statistics=false as the same setting, and --no-isolated as the opposite of --isolated.
func flagKey(arg string) (key string, ok bool) {
	name, ok := strings.CutPrefix(arg, "--")
	if !ok {
		return "", false
	}
	name, _, _ = strings.Cut(name, "=")
	name = strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(name))
	return strings.TrimPrefix(name, "no"), true
}

// undoing lists the arguments that spell a required flag in another way than the required one: a flag that is there and then said
// again as false, negated, or turned back on would pass a check that only looks for the flag, and the server would take the last
// word. A boolean word after such a flag ("--isolated false") is a value, and is listed with it.
func undoing(args, required []string) []string {
	keys := map[string]bool{}
	for _, flag := range required {
		key, _ := flagKey(flag)
		keys[key] = true
	}
	var found []string
	for i, arg := range args {
		key, ok := flagKey(arg)
		if !ok || !keys[key] {
			continue
		}
		switch {
		case i+1 < len(args) && (strings.EqualFold(args[i+1], "true") || strings.EqualFold(args[i+1], "false")):
			found = append(found, arg+" "+args[i+1])
		case !slices.Contains(required, arg):
			found = append(found, arg)
		}
	}
	return found
}

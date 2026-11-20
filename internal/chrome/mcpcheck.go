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
// profile. --headless is one of them, unless the run is headed: the person asked to see the browser, and the profile is still a
// fresh one (--isolated).
var privacyRequired = []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}

// npxFlags are the flags npx may be given before the package, none of which changes which package it runs.
var npxFlags = []string{"-y", "--yes", "--no-install", "--prefer-offline", "-q", "--quiet"}

// DefaultCommand and DefaultArgs start the pinned Chrome DevTools MCP, headless.
var (
	DefaultCommand = []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}
	DefaultArgs    = []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}
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
	return len(rest) > 0 && pinned.MatchString(rest[0])
}

// CheckMCP verifies how Chrome DevTools MCP will be started: npx on a pinned version, so a new release cannot change the
// explorer's behavior, and the flags that keep it from sending usage statistics or touching a real browser profile. A headless run
// also needs --headless; a headed one is the only run that may go without it.
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
	return errors.Join(problems...)
}

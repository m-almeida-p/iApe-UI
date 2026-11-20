package main

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"iape-ui/internal/chrome"
	"iape-ui/internal/platform/envcfg"
)

// settings are what the explorer reads from .env and the environment.
//
//	EXPLORE_BASE_URL       the Web UI (default http://localhost:8300)
//	EXPLORE_USER_EMAIL, EXPLORE_USER_PASSWORD   the user to sign in as; without them ADMIN_EMAIL and ADMIN_PASSWORD, the first user
//	                       of the Web UI, which is the usual account in development
//	EXPLORE_MCP_COMMAND    how Chrome DevTools MCP is started: npx on a pinned package (default npx -y chrome-devtools-mcp@1.10.1)
//	EXPLORE_MCP_ARGS       its flags (default --isolated --no-usage-statistics --no-performance-crux: Chrome with a window; -headless adds --headless)
type settings struct {
	BaseURL  string
	Command  []string
	Args     []string
	Email    string
	Password envcfg.Secret
}

// loadSettings reads the settings and refuses the ones that cannot be run.
func loadSettings(lookup func(string) (string, bool)) (settings, error) {
	var errs []error
	get := func(key string) (string, bool) {
		v, ok := lookup(key)
		v = strings.TrimSpace(v)
		return v, ok && v != ""
	}
	// A password is used exactly as it was given: spaces at its ends are part of it. A value of blanks alone is no password.
	getExact := func(key string) (string, bool) {
		v, ok := lookup(key)
		return v, ok && strings.TrimSpace(v) != ""
	}
	s := settings{BaseURL: "http://localhost:8300", Command: slices.Clone(chrome.DefaultCommand), Args: slices.Clone(chrome.DefaultArgs)}

	if raw, ok := get("EXPLORE_BASE_URL"); ok {
		base, err := checkBaseURL(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("EXPLORE_BASE_URL: %w", err))
		} else {
			s.BaseURL = base
		}
	}
	if raw, ok := get("EXPLORE_MCP_COMMAND"); ok {
		s.Command = strings.Fields(raw)
	}
	if raw, ok := get("EXPLORE_MCP_ARGS"); ok {
		s.Args = strings.Fields(raw)
	}
	// The headless flag is for the run to decide (-headless), so a settings file may have it or not.
	if err := chrome.CheckMCP(s.Command, s.Args, true); err != nil {
		errs = append(errs, fmt.Errorf("EXPLORE_MCP_COMMAND and EXPLORE_MCP_ARGS: %w", err))
	}

	emailKey, passwordKey := "EXPLORE_USER_EMAIL", "EXPLORE_USER_PASSWORD"
	email, hasEmail := get(emailKey)
	password, hasPassword := getExact(passwordKey)
	if !hasEmail && !hasPassword {
		emailKey, passwordKey = "ADMIN_EMAIL", "ADMIN_PASSWORD"
		email, hasEmail = get(emailKey)
		password, hasPassword = getExact(passwordKey)
	}
	switch {
	case hasEmail && !hasPassword:
		errs = append(errs, fmt.Errorf("%s is required when %s is set", passwordKey, emailKey))
	case hasPassword && !hasEmail:
		errs = append(errs, fmt.Errorf("%s is required when %s is set", emailKey, passwordKey))
	case hasEmail:
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			errs = append(errs, errors.New("the user's email must be a plain email address"))
		}
	}
	s.Email, s.Password = email, envcfg.Secret(password)
	return s, errors.Join(errs...)
}

// checkBaseURL accepts an http or https address with a host and no credentials, and says nothing of what it was given.
func checkBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return "", errors.New("must be an http or https address, such as http://localhost:8300")
	case u.User != nil:
		return "", errors.New("must not carry a user name or password")
	case strings.ContainsAny(raw, "?#"):
		// The pages are reached by adding to the address (base + "/login"), which a query or a fragment would swallow.
		return "", errors.New("must not carry a query or a fragment")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("has a port that is not between 1 and 65535")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// requireUser says there is nobody to sign in as.
func (s settings) requireUser() error {
	if s.Email == "" || s.Password.Reveal() == "" {
		return errors.New("no user to sign in as: set EXPLORE_USER_EMAIL and EXPLORE_USER_PASSWORD (or ADMIN_EMAIL and ADMIN_PASSWORD) in .env")
	}
	return nil
}

// browserArgs are the flags of the browser for a run: with a window, unless the run is headless, which is the only run that has
// --headless whatever the settings say.
func (s settings) browserArgs(headless bool) []string {
	args := slices.DeleteFunc(slices.Clone(s.Args), func(a string) bool { return a == "--headless" })
	if headless {
		args = append([]string{"--headless"}, args...)
	}
	return args
}

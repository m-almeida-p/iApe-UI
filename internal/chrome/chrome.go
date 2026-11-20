package chrome

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Errors of the Web UI as the browser sees it.
var (
	// ErrLoginFailed means the Web UI refused the sign-in, or never finished it.
	ErrLoginFailed = errors.New("signing in failed")
	// ErrNoTab means that nothing was done on the page yet: Login opens the tab every other call works in.
	ErrNoTab = errors.New("the browser has no tab: Login opens it")
	// ErrSessionEnded means the Web UI asks to sign in again: its session has a lifetime.
	ErrSessionEnded = errors.New("the Web UI session ended")
)

// ChromeConfig says how a Chrome behaves.
type ChromeConfig struct {
	// Viewport is the size of the window as the emulate tool spells it; zero means a laptop, 1280x800x1.
	Viewport string
	// Poll is how often a page is looked at while waiting; zero means 250 ms.
	Poll time.Duration
	// Settle is how long a page may take to show what it should after a navigation or a click; zero means 15 seconds.
	Settle time.Duration
}

// DefaultViewport is a laptop screen.
const DefaultViewport = "1280x800x1"

// Chrome is one browser tab on the Web UI, driven through Chrome DevTools MCP the way a person uses it: elements are found by their
// accessible names. It is not safe for concurrent use.
type Chrome struct {
	s    *Session
	tab  tab
	base string
	cfg  ChromeConfig
}

// NewChrome wraps a session. The tab is opened by Login. baseURL is the Web UI, such as http://localhost:8300; it may carry
// credentials, which no error shows.
func NewChrome(s *Session, baseURL string, cfg ChromeConfig) *Chrome {
	return &Chrome{s: s, tab: tab{s: s}, base: strings.TrimRight(baseURL, "/"), cfg: cfg}
}

// Login opens a tab on the sign-in page and signs in.
func (c *Chrome) Login(ctx context.Context, email, password string) error {
	res, err := c.s.Call(ctx, "new_page", map[string]any{"url": c.base + "/login"})
	if err != nil {
		return fmt.Errorf("opening the Web UI at %s: %w", redactURL(c.base), err)
	}
	page, ok := SelectedPage(ParsePages(res.Text))
	if !ok {
		return fmt.Errorf("opening the Web UI: the browser reported no page: %q", shorten(res.Text, 120))
	}
	c.tab.id = page.ID
	if _, err := c.tab.call(ctx, "emulate", map[string]any{"viewport": cmp.Or(c.cfg.Viewport, DefaultViewport)}); err != nil {
		return fmt.Errorf("setting the size of the browser window: %w", err)
	}

	snap, err := c.tab.snapshot(ctx)
	if err != nil {
		return err
	}
	emailBox, err := snap.Require(Query{Role: "textbox", Name: "Email"})
	if err != nil {
		if composerOf(snap) {
			return nil // the Web UI took the browser past the sign-in page: it is signed in already
		}
		return fmt.Errorf("the sign-in page: %w", err)
	}
	passwordBox, err := snap.Require(Query{Role: "textbox", Name: "Password"})
	if err != nil {
		return fmt.Errorf("the sign-in page: %w", err)
	}
	if err := c.tab.fill(ctx, emailBox.UID, email); err != nil {
		return fmt.Errorf("typing the email: %w", err)
	}
	if err := c.tab.fill(ctx, passwordBox.UID, password); err != nil {
		return fmt.Errorf("typing the password: %w", err)
	}
	if err := c.tab.clickNamed(ctx, Query{Role: "button", Name: "Sign in"}); err != nil {
		return fmt.Errorf("pressing Sign in: %w", err)
	}
	if err := c.waitFor(ctx, func(snap Snapshot) bool { return composerOf(snap) }); err != nil {
		return fmt.Errorf("%w: the page did not leave the sign-in form (%w)", ErrLoginFailed, err)
	}
	return nil
}

// StartChat opens the start page, where a message starts a new conversation, and waits for its composer.
func (c *Chrome) StartChat(ctx context.Context) error {
	if err := c.tab.navigate(ctx, c.base+"/", ""); err != nil {
		return fmt.Errorf("opening the start page: %w", err)
	}
	err := c.waitFor(ctx, func(snap Snapshot) bool { return composerOf(snap) || signInOf(snap) })
	if err != nil {
		return fmt.Errorf("waiting for the composer: %w", err)
	}
	snap, err := c.tab.snapshot(ctx)
	if err != nil {
		return err
	}
	if signInOf(snap) {
		return fmt.Errorf("%w: the page asks to sign in again", ErrSessionEnded)
	}
	return nil
}

func composerOf(snap Snapshot) bool {
	_, ok := snap.Find(Query{Role: "textbox", Name: "Message"})
	return ok
}

func signInOf(snap Snapshot) bool {
	_, ok := snap.Find(Query{Role: "textbox", Name: "Password"})
	return ok
}

// waitFor looks at the page until ok says it is what was waited for.
func (c *Chrome) waitFor(ctx context.Context, ok func(Snapshot) bool) error {
	deadline := time.Now().Add(cmp.Or(c.cfg.Settle, 15*time.Second))
	for {
		snap, err := c.tab.snapshot(ctx)
		if err == nil && ok(snap) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("the page did not show it in time")
		}
		if err := pauseBefore(ctx, cmp.Or(c.cfg.Poll, 250*time.Millisecond), deadline); err != nil {
			return err
		}
	}
}

// Tool calls a Chrome DevTools MCP tool on the tab.
func (c *Chrome) Tool(ctx context.Context, name string, args map[string]any, opts ...CallOption) (Result, error) {
	return c.tab.call(ctx, name, args, opts...)
}

// Click clicks the element a query finds in a fresh snapshot.
func (c *Chrome) Click(ctx context.Context, q Query) error { return c.tab.clickNamed(ctx, q) }

// Fill puts text in the element a query finds.
func (c *Chrome) Fill(ctx context.Context, q Query, value string) error {
	snap, err := c.tab.snapshot(ctx)
	if err != nil {
		return err
	}
	node, err := snap.Require(q)
	if err != nil {
		return err
	}
	return c.tab.fill(ctx, node.UID, value)
}

// Eval runs a function in the page and decodes the JSON it returns into out.
func (c *Chrome) Eval(ctx context.Context, function string, out any) error {
	return c.tab.eval(ctx, function, out)
}

// Screenshot saves the page as a PNG at path, readable by its owner alone.
func (c *Chrome) Screenshot(ctx context.Context, path string) error {
	res, err := c.tab.call(ctx, "take_screenshot", map[string]any{"format": "png"})
	if err != nil {
		return err
	}
	// A picture of no bytes is no picture: written out, it would be an empty file that looks like a screenshot.
	i := slices.IndexFunc(res.Images, func(img Image) bool { return len(img.Data) > 0 })
	if i < 0 {
		return errors.New("the browser returned no picture")
	}
	return os.WriteFile(path, res.Images[i].Data, 0o600)
}

// Close ends the browser session.
func (c *Chrome) Close(ctx context.Context) error { return c.s.Close(ctx) }

// redactURL is an address without the credentials it may carry.
func redactURL(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" { // "user:password@host" parses, with a scheme and no host, and prints as it came
		return "(an address)"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	return u.String()
}

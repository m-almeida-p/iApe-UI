package main

import (
	"context"
	"time"

	"iape-ui/internal/chrome"
	"iape-ui/internal/explore"
)

// session is a browser that is signed in, and can start a conversation of its own for each path.
type session interface {
	explore.Browser
	// Fresh opens the start page, where the next message starts a new conversation.
	Fresh(ctx context.Context) error
	Close(ctx context.Context) error
}

// openOptions say how a run opens its browser.
type openOptions struct {
	// Headless runs Chrome without a window. The default is a window, to watch the run.
	Headless bool
	// Settle is how long a step waits for the page to settle.
	Settle time.Duration
}

// chromeSession is the real session: Chrome, through Chrome DevTools MCP.
type chromeSession struct {
	*chrome.Browser
	chrome *chrome.Chrome
}

func (s *chromeSession) Fresh(ctx context.Context) error { return s.chrome.StartChat(ctx) }
func (s *chromeSession) Close(ctx context.Context) error { return s.chrome.Close(ctx) }

// openSession starts Chrome and signs in.
func openSession(ctx context.Context, s settings, o openOptions) (session, error) {
	mcp, err := chrome.Open(ctx, chrome.Options{Command: s.Command, Args: s.browserArgs(o.Headless), Headed: !o.Headless})
	if err != nil {
		return nil, err
	}
	c := chrome.NewChrome(mcp, s.BaseURL, chrome.ChromeConfig{})
	if err := c.Login(ctx, s.Email, s.Password.Reveal()); err != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return &chromeSession{Browser: &chrome.Browser{Driver: c, SettleWithin: o.Settle}, chrome: c}, nil
}

package main

import (
	"context"
	"time"

	"iape-ui/internal/eval/infrastructure/config"
	"iape-ui/internal/eval/infrastructure/devtools"
	"iape-ui/internal/explore"
	"iape-ui/internal/explore/infrastructure/chrome"
)

// session is a browser that is signed in, and can start a conversation of its own for each path.
type session interface {
	explore.Browser
	// Fresh opens the start page, where the next message starts a new conversation.
	Fresh(ctx context.Context) error
	Close(ctx context.Context) error
}

// chromeSession is the real session: Chrome, through the evaluator's driver.
type chromeSession struct {
	*chrome.Browser
	driver *devtools.Driver
}

func (s *chromeSession) Fresh(ctx context.Context) error { return s.driver.StartChat(ctx) }
func (s *chromeSession) Close(ctx context.Context) error { return s.driver.Close(ctx) }

// openSession starts Chrome and signs in as the user of the evaluator.
func openSession(ctx context.Context, cfg config.Config, settle time.Duration) (session, error) {
	factory := &devtools.Factory{
		Session: devtools.Options{Command: cfg.MCP.Command, Args: cfg.MCP.Args},
		Driver:  devtools.DriverConfig{BaseURL: cfg.Eval.BaseURL},
	}
	driver, err := factory.Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := driver.Login(ctx, cfg.Eval.UserEmail, cfg.Eval.UserPassword.Reveal()); err != nil {
		_ = driver.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return &chromeSession{Browser: &chrome.Browser{Driver: driver, SettleWithin: settle}, driver: driver}, nil
}

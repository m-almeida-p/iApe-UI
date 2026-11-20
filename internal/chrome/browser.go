// Package chrome is the explorer's browser: it drives the Web UI in Chrome through the evaluator's driver, finding elements by their
// accessible names the way a person who cannot see the page does.
package chrome

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"iape-ui/internal/explore"
)

// Driver is what the browser needs of the evaluator's driver, which *Driver is.
type Driver interface {
	Tool(ctx context.Context, name string, args map[string]any, opts ...CallOption) (Result, error)
	Click(ctx context.Context, q Query) error
	Fill(ctx context.Context, q Query, value string) error
	Eval(ctx context.Context, function string, out any) error
	Screenshot(ctx context.Context, path string) error
}

// The page's own words: the accessible names of what the explorer touches.
const (
	composerName = "Message"
	sendName     = "Send message"
	codeName     = "Code from the bank"
)

// Scripts the page runs. They read; they never change the page.
const (
	composerValue = `() => document.querySelector('#composer-input')?.value ?? ''`
	// busy is true while a reply is being written or a request of htmx is out.
	busyScript  = `() => !!document.querySelector('[data-role="assistant"]:is([data-status="pending"], [data-status="streaming"]), .htmx-request')`
	cardsScript = `() => [...document.querySelectorAll('[data-testid="card"]')].map((c) => ({ kind: c.dataset.kind ?? '', state: c.dataset.state ?? '' }))`
)

// Browser implements explore.Browser over a driver whose tab is signed in and on a page of the Web UI.
type Browser struct {
	Driver Driver
	// Poll is how often Settle and Pick look; zero means 250 ms.
	Poll time.Duration
	// SettleWithin is how long a page may keep a reply in flight before Settle gives up; zero means 90 seconds.
	SettleWithin time.Duration
	// PickWithin is how long Pick waits for the picker to show its options; zero means 5 seconds.
	PickWithin time.Duration
}

var _ explore.Browser = (*Browser)(nil)

func (b *Browser) poll() time.Duration { return cmp.Or(b.Poll, 250*time.Millisecond) }

// Type implements explore.Browser. The composer is filled with what it holds and the text, so that "@(Juan Perez) " and then "20
// soles" make one message.
func (b *Browser) Type(ctx context.Context, text string) error {
	var current string
	if err := b.Driver.Eval(ctx, composerValue, &current); err != nil {
		return fmt.Errorf("reading the composer: %w", err)
	}
	if err := b.Driver.Fill(ctx, Query{Role: "textbox", Name: composerName}, current+text); err != nil {
		return fmt.Errorf("typing in the composer: %w", err)
	}
	return nil
}

// Send implements explore.Browser.
func (b *Browser) Send(ctx context.Context) error {
	if err := b.Driver.Click(ctx, Query{Role: "button", Name: sendName}); err != nil {
		return fmt.Errorf("pressing Send: %w", err)
	}
	return nil
}

// roles are what a step may press, in the order they are looked for: a card's buttons, the picker's options, links.
var roles = []string{"button", "option", "link"}

// Click implements explore.Browser.
func (b *Browser) Click(ctx context.Context, text string) (bool, error) {
	for _, role := range roles {
		err := b.Driver.Click(ctx, Query{Role: role, NameContains: text})
		if err == nil {
			return true, nil
		}
		if _, missing := errors.AsType[*NotFoundError](err); !missing {
			return false, fmt.Errorf("pressing %q: %w", text, err)
		}
	}
	return false, nil
}

// Pick implements explore.Browser. The picker shows its options a moment after the @ is typed, so it looks for them for a while.
func (b *Browser) Pick(ctx context.Context, text string) error {
	within := cmp.Or(b.PickWithin, 5*time.Second)
	deadline := time.Now().Add(within)
	for {
		err := b.Driver.Click(ctx, Query{Role: "option", NameContains: text})
		if err == nil {
			return b.afterPick(ctx)
		}
		if _, missing := errors.AsType[*NotFoundError](err); !missing {
			return fmt.Errorf("choosing a contact in the picker: %w", err)
		}
		if time.Now().After(deadline) {
			if text == "" {
				return fmt.Errorf("the contact picker showed no contact within %s", within)
			}
			return fmt.Errorf("the contact picker did not show %q within %s", text, within)
		}
		if err := pause(ctx, b.poll()); err != nil {
			return err
		}
	}
}

// afterPick waits for the choice to be confirmed with the agent's service and put in the message: the composer holds @(Name) then.
func (b *Browser) afterPick(ctx context.Context) error {
	return b.Settle(ctx)
}

// FillOTP implements explore.Browser. Enter in the field confirms the code.
func (b *Browser) FillOTP(ctx context.Context, code string) error {
	if err := b.Driver.Fill(ctx, Query{Role: "textbox", Name: codeName}, code); err != nil {
		return fmt.Errorf("typing the code: %w", err)
	}
	if _, err := b.Driver.Tool(ctx, "press_key", map[string]any{"key": "Enter"}); err != nil {
		return fmt.Errorf("confirming the code: %w", err)
	}
	return nil
}

// quiet is how many looks in a row must find the page idle for it to be settled: a reply that has just ended may be followed by
// the swap of a card.
const quiet = 2

// Settle implements explore.Browser.
func (b *Browser) Settle(ctx context.Context) error {
	limit := cmp.Or(b.SettleWithin, 90*time.Second)
	deadline := time.Now().Add(limit)
	idle := 0
	for {
		var busy bool
		if err := b.Driver.Eval(ctx, busyScript, &busy); err != nil {
			return fmt.Errorf("looking at the page: %w", err)
		}
		if busy {
			idle = 0
		} else if idle++; idle >= quiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the page still shows a reply being written after %s", limit)
		}
		if err := pause(ctx, b.poll()); err != nil {
			return err
		}
	}
}

// Text implements explore.Browser.
func (b *Browser) Text(ctx context.Context) (string, error) {
	res, err := b.Driver.Tool(ctx, "take_snapshot", nil)
	return res.Text, err
}

// Cards implements explore.Browser.
func (b *Browser) Cards(ctx context.Context) ([]explore.Card, error) {
	var raw []struct {
		Kind  string `json:"kind"`
		State string `json:"state"`
	}
	if err := b.Driver.Eval(ctx, cardsScript, &raw); err != nil {
		return nil, err
	}
	cards := make([]explore.Card, len(raw))
	for i, c := range raw {
		cards[i] = explore.Card{Kind: c.Kind, State: c.State}
	}
	return cards, nil
}

// Requests implements explore.Browser.
func (b *Browser) Requests(ctx context.Context) (string, error) {
	res, err := b.Driver.Tool(ctx, "list_network_requests", nil)
	return res.Text, err
}

// Screenshot implements explore.Browser.
func (b *Browser) Screenshot(ctx context.Context, path string) error {
	return b.Driver.Screenshot(ctx, path)
}

func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

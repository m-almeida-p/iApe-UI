// Package explore is the explorer's logic: the language of its steps, the catalog of the paths through the P2P flows, and the run
// that follows steps through a page and records what the page shows after each one. It knows nothing of how the page is reached
// (a Browser does that) or where the records go (a Sink does), and nothing of the Web UI itself: it reads text and the kind and
// state of the cards on the page.
package explore

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Kind is what a step does.
type Kind string

// The steps there are.
const (
	// KindMessage sends text as a message: it is typed in the composer, sent, and the run waits for the page to settle.
	KindMessage Kind = "message"
	// KindType types text into the composer, after what is there, without sending.
	KindType Kind = "type"
	// KindSend sends what the composer holds.
	KindSend Kind = "send"
	// KindPick chooses a contact in the @ picker: the first one, or the first whose line has the text.
	KindPick Kind = "pick"
	// KindClick presses the first button, option or link whose name has the text; none is an error.
	KindClick Kind = "click"
	// KindClickIf is KindClick that lets a page that has no such element be.
	KindClickIf Kind = "click?"
	// KindWait pauses without touching the page, like a person reading.
	KindWait Kind = "wait"
	// KindOTP types the code the bank asked for into the card's field and confirms it.
	KindOTP Kind = "otp"
	// KindExpect fails the run unless the page shows the text.
	KindExpect Kind = "expect"
	// KindExpectCard fails the run unless the newest card of a kind is in a state.
	KindExpectCard Kind = "expect-card"
	// KindExpectNoCard fails the run if the page shows any card.
	KindExpectNoCard Kind = "expect-no-card"
)

// Card is the kind and the state of a card on the page.
type Card struct{ Kind, State string }

func (c Card) String() string { return c.Kind + "=" + c.State }

// cardStates says which states each kind of card has: the explorer refuses to expect one that no card has.
var cardStates = map[string][]string{
	"approval": {"pending", "otp_required", "paid", "rejected"},
	"contact":  {"pending", "confirmed"},
}

// Bounds of a pause: more than that is a hung run, not a person reading.
const maxWait = 5 * time.Minute

// Step is one thing the run does.
type Step struct {
	Kind Kind
	// Arg is the text of the step: what to send, type, click, look for, or the code.
	Arg string
	// Wait is how long a KindWait pauses.
	Wait time.Duration
	// Card is what a KindExpectCard looks for.
	Card Card
	// Raw is the step as it was written, for the records.
	Raw string
}

// Parse reads a step. A line that is no step by its prefix is a message to the agent, so that "Hola" is a step.
//
//	@               types "@" (opens the contact picker)
//	@pick[:text]    chooses a contact in the picker: the first, or the first with the text
//	type:text       types text after what the composer holds, without sending
//	send            sends what the composer holds
//	click:text      presses the button, option or link with the text in its name
//	click?:text     the same, but a page without it is fine
//	wait:8s         pauses
//	otp:123456      types the code into the card's field and confirms it
//	expect:text     fails unless the page shows the text
//	expect-card:kind=state   fails unless the newest card of the kind is in the state
//	expect-no-card  fails if the page shows a card
//	anything else   is sent as a message
func Parse(raw string) (Step, error) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return Step{}, fmt.Errorf("an empty step")
	}
	s := Step{Raw: raw}
	switch {
	case line == "@":
		s.Kind, s.Arg = KindType, "@"
	case line == "@pick":
		s.Kind = KindPick
	case strings.HasPrefix(line, "@pick:"):
		s.Kind, s.Arg = KindPick, strings.TrimSpace(strings.TrimPrefix(line, "@pick:"))
	case line == "send":
		s.Kind = KindSend
	case line == "expect-no-card":
		s.Kind = KindExpectNoCard
	case strings.HasPrefix(line, "type:"):
		s.Kind, s.Arg = KindType, strings.TrimSpace(strings.TrimPrefix(line, "type:"))
		if s.Arg == "" {
			return Step{}, fmt.Errorf("type: has nothing to type")
		}
	case strings.HasPrefix(line, "click?:"):
		s.Kind, s.Arg = KindClickIf, strings.TrimSpace(strings.TrimPrefix(line, "click?:"))
		if s.Arg == "" {
			return Step{}, fmt.Errorf("click?: has nothing to click")
		}
	case strings.HasPrefix(line, "click:"):
		s.Kind, s.Arg = KindClick, strings.TrimSpace(strings.TrimPrefix(line, "click:"))
		if s.Arg == "" {
			return Step{}, fmt.Errorf("click: has nothing to click")
		}
	case strings.HasPrefix(line, "wait:"):
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(line, "wait:")))
		if err != nil {
			return Step{}, fmt.Errorf("wait: needs a duration such as 8s: %w", err)
		}
		if d <= 0 || d > maxWait {
			return Step{}, fmt.Errorf("wait: takes a duration between 0 and %s, not %s", maxWait, d)
		}
		s.Kind, s.Wait = KindWait, d
	case strings.HasPrefix(line, "otp:"):
		code := strings.TrimSpace(strings.TrimPrefix(line, "otp:"))
		if n := len(code); n < 4 || n > 8 || strings.IndexFunc(code, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
			return Step{}, fmt.Errorf("otp: takes 4 to 8 digits")
		}
		s.Kind, s.Arg = KindOTP, code
	case strings.HasPrefix(line, "expect:"):
		s.Kind, s.Arg = KindExpect, strings.TrimSpace(strings.TrimPrefix(line, "expect:"))
		if s.Arg == "" {
			return Step{}, fmt.Errorf("expect: has nothing to look for")
		}
	case strings.HasPrefix(line, "expect-card:"):
		card, err := parseCard(strings.TrimPrefix(line, "expect-card:"))
		if err != nil {
			return Step{}, err
		}
		s.Kind, s.Card = KindExpectCard, card
	default:
		s.Kind, s.Arg = KindMessage, line
	}
	return s, nil
}

func parseCard(text string) (Card, error) {
	kind, state, ok := strings.Cut(strings.TrimSpace(text), "=")
	if !ok || kind == "" || state == "" {
		return Card{}, fmt.Errorf("expect-card: takes kind=state, such as approval=paid")
	}
	states, known := cardStates[kind]
	if !known {
		return Card{}, fmt.Errorf("expect-card: there is no kind %q of card (%s)", kind, strings.Join(kinds(), ", "))
	}
	if !slices.Contains(states, state) {
		return Card{}, fmt.Errorf("expect-card: a %s card has no state %q (%s)", kind, state, strings.Join(states, ", "))
	}
	return Card{Kind: kind, State: state}, nil
}

func kinds() []string {
	out := make([]string, 0, len(cardStates))
	for k := range cardStates {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ParseAll reads steps, naming the one that cannot be read.
func ParseAll(raws []string) ([]Step, error) {
	steps := make([]Step, 0, len(raws))
	for i, raw := range raws {
		s, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("step %d (%q): %w", i+1, raw, err)
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// StepForm describes one form of a step, for the help of the command: how it is written, what it does, and an example that Parse
// reads (a test keeps that true).
type StepForm struct {
	Form    string
	Does    string
	Example string
}

// StepForms are the steps there are, in the order the help shows them.
var StepForms = []StepForm{
	{"<text>", "send it as a message to the agent (anything that is no step below)", "quiero yapear"},
	{"@", "type @ in the message box: the contact picker opens", "@"},
	{"@pick[:<name>]", "choose a contact in the picker: the first, or the first whose line has the name", "@pick:Juan Perez"},
	{"type:<text>", "type text after what the message box holds, without sending", "type:20 soles"},
	{"send", "send what the message box holds", "send"},
	{"click:<text>", "press the button, option or link with the text in its name; none is a failure", "click:Pay"},
	{"click?:<text>", "the same, but a page without it is fine", "click?:Pay"},
	{"wait:<duration>", "pause without touching the page, like a person reading (up to 5m)", "wait:8s"},
	{"otp:<code>", "type the code the bank asked for (4 to 8 digits) in the card's field and confirm it", "otp:123456"},
	{"expect:<text>", "fail unless the page shows the text", "expect:S/500"},
	{"expect-card:<kind>=<state>", "fail unless the newest card of that kind is in that state (kinds: approval, contact)", "expect-card:approval=paid"},
	{"expect-no-card", "fail if the page shows any card", "expect-no-card"},
}

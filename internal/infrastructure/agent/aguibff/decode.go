package aguibff

import (
	"bufio"
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"

	"iape-ui/internal/core/domain"
)

// maxLine is the longest event line accepted, and the longest event: one made of many lines is as big as their sum. A tool result
// can be large, a runaway stream cannot.
const maxLine = 4 << 20

// byteOrderMark is the one a stream may begin with; the SSE spec says to drop it.
const byteOrderMark = "\ufeff"

// header is what every AG-UI event has. The rest of an event is read by the type that needs it: an event the Web UI does not work
// with (STATE_DELTA, whose delta is a list, not a string) cannot break the stream by the shape of its fields.
type header struct {
	Type string `json:"type"`
}

type runStartedEvent struct {
	RunID string `json:"runId"`
}

type contentEvent struct {
	Delta string `json:"delta"`
}

type toolCallEvent struct {
	Tool string `json:"toolCallName"`
}

type runErrorEvent struct {
	Message jsontext.Value `json:"message"`
}

// customEvent is a CUSTOM event. Value is read only for the cards the Web UI shows.
type customEvent struct {
	Name  string         `json:"name"`
	Value jsontext.Value `json:"value"`
}

// actionRequest is the value of the CUSTOM event ACTION_REQUEST: a transfer waiting for the user's answer.
type actionRequest struct {
	ActionType string `json:"actionType"`
	ApprovalID string `json:"approvalId"`
	Preview    struct {
		Amount           float64 `json:"amount"`
		Currency         string  `json:"currency"`
		DestinationName  string  `json:"destinationName"`
		DestinationPhone string  `json:"destinationPhone"`
		Message          string  `json:"message"`
	} `json:"preview"`
}

// parse reads the fields of an event that a type needs.
func parse[T any](data string) (T, error) {
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return v, fmt.Errorf("malformed event: %w", err)
	}
	return v, nil
}

// messageOf is the text of a RUN_ERROR's message. A message that is not text is no message: the run still failed.
func messageOf(v jsontext.Value) string {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// Decode reads the SSE of an AG-UI run and yields what the Web UI works with: the text as deltas, each message closed by a
// final that replaces its deltas, and the tools the agent called. Anything else in the stream is left alone.
//
// An event is what the SSE spec says: its data lines, joined by line feeds, up to the blank line that ends it (the end of the
// stream ends the last one). The BFF may close the run before the last message ends (RUN_FINISHED, then TEXT_MESSAGE_END), so
// the end of the stream, not RUN_FINISHED, ends the reading. A message that starts while another is open closes the other
// with its own final. A RUN_ERROR is the agent's own error. A stream that ends without RUN_FINISHED, a broken event and an
// event over four megabytes are errors that wrap domain.ErrAgentUnavailable.
func Decode(r io.Reader) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64<<10), maxLine)

		var segment strings.Builder
		var runID string
		open, finished, delivered := false, false, false
		fail := func(format string, args ...any) {
			yield(domain.AgentEvent{}, fmt.Errorf("%w: "+format, append([]any{domain.ErrAgentUnavailable}, args...)...))
		}
		tooLong := func() { fail("an event line is too long (over %d bytes)", maxLine) }

		// handle does what one event asks. It is false when the reading is over: the consumer left, or the stream failed.
		handle := func(data string) bool {
			head, err := parse[header](data)
			if err != nil {
				fail("%w", err)
				return false
			}

			switch head.Type {
			case "RUN_STARTED":
				ev, err := parse[runStartedEvent](data)
				if err != nil {
					fail("%w", err)
					return false
				}
				runID = ev.RunID
			case "CUSTOM":
				ev, err := parse[customEvent](data)
				if err != nil {
					fail("%w", err)
					return false
				}
				card, ok, err := cardOf(ev, runID)
				if err != nil {
					fail("%w", err)
					return false
				}
				if ok && !yield(domain.AgentEvent{Kind: domain.AgentInteraction, Interaction: card}, nil) {
					return false
				}
			case "TEXT_MESSAGE_START":
				if open && !yield(domain.AgentEvent{Kind: domain.AgentFinal, Text: segment.String()}, nil) {
					return false
				}
				segment.Reset()
				open = true
			case "TEXT_MESSAGE_CONTENT":
				ev, err := parse[contentEvent](data)
				if err != nil {
					fail("%w", err)
					return false
				}
				open = true
				segment.WriteString(ev.Delta)
				if ev.Delta != "" {
					delivered = true
					if !yield(domain.AgentEvent{Kind: domain.AgentDelta, Text: ev.Delta}, nil) {
						return false
					}
				}
			case "TEXT_MESSAGE_END":
				if open && !yield(domain.AgentEvent{Kind: domain.AgentFinal, Text: segment.String()}, nil) {
					return false
				}
				open = false
			case "TOOL_CALL_START":
				ev, err := parse[toolCallEvent](data)
				if err != nil {
					fail("%w", err)
					return false
				}
				if !yield(domain.AgentEvent{Kind: domain.AgentToolCall, Tool: ev.Tool}, nil) {
					return false
				}
			case "RUN_FINISHED":
				finished = true
			case "RUN_ERROR":
				ev, _ := parse[runErrorEvent](data) // the type was read: a message that cannot be read is an empty one
				yield(domain.AgentEvent{}, &domain.AgentError{Detail: messageOf(ev.Message)})
				return false
			}
			return true
		}

		var lines []string // the data lines of the event being read
		size := 0
		dispatch := func() bool {
			data := strings.Join(lines, "\n")
			lines, size = lines[:0], 0
			return data == "" || handle(data) // an event without data is a keep-alive
		}

		first := true
		for scanner.Scan() {
			line := scanner.Text()
			if first {
				line, first = strings.TrimPrefix(line, byteOrderMark), false
			}
			if line == "" { // the blank line that ends an event
				if !dispatch() {
					return
				}
				continue
			}
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue // a comment, or an `event:`, `id:` or `retry:` line
			}
			if size += len(data) + 1; size > maxLine {
				tooLong()
				return
			}
			lines = append(lines, strings.TrimPrefix(data, " "))
		}

		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				tooLong()
				return
			}
			if delivered { // a connection that dies after some text is an answer that stopped short, like a stream that ends
				yield(domain.AgentEvent{}, fmt.Errorf("%w: reading the stream: %w", domain.ErrIncompleteStream, err))
				return
			}
			fail("reading the stream: %w", err)
			return
		}
		if !dispatch() { // the last event, when the stream ended without its blank line
			return
		}
		if !finished {
			// A stream cut after some text is an answer that stopped short; one cut before any is an agent that did not answer.
			if delivered {
				yield(domain.AgentEvent{}, fmt.Errorf("%w: the stream ended before RUN_FINISHED", domain.ErrIncompleteStream))
				return
			}
			fail("the stream ended before RUN_FINISHED")
			return
		}
		if open { // a message the stream never closed
			yield(domain.AgentEvent{Kind: domain.AgentFinal, Text: segment.String()}, nil)
		}
	}
}

// contactRequest is the value of the CUSTOM event CONTACT_REQUEST: the agent asks the user to confirm who to pay.
type contactRequest struct {
	RequestID      string `json:"requestId"`
	Query          string `json:"query"`
	AgentReference string `json:"agentReference"`
	Message        string `json:"message"`
}

// cardOf is the card of a CUSTOM event, stamped with the run that made it: a transfer to approve (ACTION_REQUEST) or a contact to
// confirm (CONTACT_REQUEST). Any other CUSTOM event, and an ACTION_REQUEST for another kind of action, is not a card: ok is false.
func cardOf(ev customEvent, runID string) (card domain.Interaction, ok bool, err error) {
	switch ev.Name {
	case "ACTION_REQUEST":
		return approvalCard(ev, runID)
	case "CONTACT_REQUEST":
		return contactCard(ev, runID)
	}
	return card, false, nil
}

// contactCard is the card of a CONTACT_REQUEST. What the BFF wants back when the contact is confirmed is the agent's reference,
// and the query when it gave none.
func contactCard(ev customEvent, runID string) (card domain.Interaction, ok bool, err error) {
	var req contactRequest
	if err := json.Unmarshal(ev.Value, &req); err != nil {
		return card, false, fmt.Errorf("malformed CONTACT_REQUEST: %w", err)
	}
	query := cmp.Or(req.AgentReference, req.Query)
	if query == "" {
		return card, false, errors.New("a contact request without query")
	}
	return domain.Interaction{
		Kind:      domain.InteractionContact,
		State:     domain.InteractionPending,
		RequestID: req.RequestID,
		RunID:     runID,
		Query:     query,
		Prompt:    req.Message,
	}, true, nil
}

// approvalCard is the card of an ACTION_REQUEST for a transfer.
func approvalCard(ev customEvent, runID string) (card domain.Interaction, ok bool, err error) {
	var req actionRequest
	if err := json.Unmarshal(ev.Value, &req); err != nil {
		return card, false, fmt.Errorf("malformed ACTION_REQUEST: %w", err)
	}
	if req.ActionType != "approval" {
		return card, false, nil
	}
	if req.ApprovalID == "" {
		return card, false, errors.New("an approval card without approvalId")
	}
	return domain.Interaction{
		Kind:       domain.InteractionApproval,
		State:      domain.InteractionPending,
		ApprovalID: req.ApprovalID,
		RunID:      runID,
		Transfer: domain.Transfer{
			Amount:           req.Preview.Amount,
			Currency:         req.Preview.Currency,
			DestinationName:  req.Preview.DestinationName,
			DestinationPhone: req.Preview.DestinationPhone,
			Message:          req.Preview.Message,
		},
	}, true, nil
}

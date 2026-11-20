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

// maxLine is the longest event line accepted. A tool result can be large, a runaway stream cannot.
const maxLine = 4 << 20

// event is the part of an AG-UI event the decoder reads. The rest is left to what needs it (the cards).
type event struct {
	Type    string `json:"type"`
	Delta   string `json:"delta"`
	Tool    string `json:"toolCallName"`
	Message string `json:"message"`
	RunID   string `json:"runId"`
	// Name and Value are those of a CUSTOM event. Value is read only for the cards the Web UI shows.
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

// Decode reads the SSE of an AG-UI run and yields what the Web UI works with: the text as deltas, each message closed by a
// final that replaces its deltas, and the tools the agent called. Anything else in the stream is left alone.
//
// The BFF may close the run before the last message ends (RUN_FINISHED, then TEXT_MESSAGE_END), so the end of the stream, not
// RUN_FINISHED, ends the reading. A RUN_ERROR is the agent's own error. A stream that ends without RUN_FINISHED, a broken
// event and a line over four megabytes are errors that wrap domain.ErrAgentUnavailable.
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

		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data:")
			if !ok {
				continue // a comment, an `event:` line or the blank line between events
			}
			var ev event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(data, " ")), &ev); err != nil {
				fail("malformed event: %w", err)
				return
			}

			switch ev.Type {
			case "RUN_STARTED":
				runID = ev.RunID
			case "CUSTOM":
				card, ok, err := cardOf(ev, runID)
				if err != nil {
					fail("%w", err)
					return
				}
				if ok && !yield(domain.AgentEvent{Kind: domain.AgentInteraction, Interaction: card}, nil) {
					return
				}
			case "TEXT_MESSAGE_START":
				segment.Reset()
				open = true
			case "TEXT_MESSAGE_CONTENT":
				open = true
				segment.WriteString(ev.Delta)
				if ev.Delta != "" {
					delivered = true
					if !yield(domain.AgentEvent{Kind: domain.AgentDelta, Text: ev.Delta}, nil) {
						return
					}
				}
			case "TEXT_MESSAGE_END":
				if open && !yield(domain.AgentEvent{Kind: domain.AgentFinal, Text: segment.String()}, nil) {
					return
				}
				open = false
			case "TOOL_CALL_START":
				if !yield(domain.AgentEvent{Kind: domain.AgentToolCall, Tool: ev.Tool}, nil) {
					return
				}
			case "RUN_FINISHED":
				finished = true
			case "RUN_ERROR":
				yield(domain.AgentEvent{}, &domain.AgentError{Detail: ev.Message})
				return
			}
		}

		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				fail("an event line is too long (over %d bytes)", maxLine)
				return
			}
			fail("reading the stream: %w", err)
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
func cardOf(ev event, runID string) (card domain.Interaction, ok bool, err error) {
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
func contactCard(ev event, runID string) (card domain.Interaction, ok bool, err error) {
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
func approvalCard(ev event, runID string) (card domain.Interaction, ok bool, err error) {
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

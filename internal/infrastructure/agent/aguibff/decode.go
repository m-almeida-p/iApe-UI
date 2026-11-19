package aguibff

import (
	"bufio"
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
		open, finished := false, false
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
			case "TEXT_MESSAGE_START":
				segment.Reset()
				open = true
			case "TEXT_MESSAGE_CONTENT":
				open = true
				segment.WriteString(ev.Delta)
				if ev.Delta != "" && !yield(domain.AgentEvent{Kind: domain.AgentDelta, Text: ev.Delta}, nil) {
					return
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
			fail("the stream ended before RUN_FINISHED")
			return
		}
		if open { // a message the stream never closed
			yield(domain.AgentEvent{Kind: domain.AgentFinal, Text: segment.String()}, nil)
		}
	}
}

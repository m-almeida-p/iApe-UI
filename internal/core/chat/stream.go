package chat

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/service"
)

// Stream implements service.Chat.
//
// Pattern: State-Dispatched Stream
func (s *Service) Stream(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) iter.Seq2[service.StreamEvent, error] {
	return func(yield func(service.StreamEvent, error) bool) {
		msg, err := s.deps.Live.Get(userID, convID, msgID)
		if err != nil {
			yield(service.StreamEvent{}, err)
			return
		}

		switch {
		case msg.Status.Terminal():
			// Already produced (a reconnect): replay it as one final event, without bothering the agent again. The event carries the
			// whole text, so no snapshot comes first.
			yield(doneEvent(msg.Status, msg.Content, msg.Error, msg.Meta), nil)
			return
		case msg.Status == domain.StatusStreaming:
			if _, writing := s.runs.get(msgID); writing {
				yield(service.StreamEvent{}, domain.ErrAlreadyStreaming) // another connection is on it
				return
			}
			ended, err := s.endLost(userID, msg)
			if err != nil {
				yield(service.StreamEvent{}, err)
				return
			}
			yield(doneEvent(ended.Status, ended.Content, ended.Error, ended.Meta), nil)
			return
		}

		// Register before starting, so a stop request that arrives in between finds this run.
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		active, ok := s.runs.begin(msgID, cancel)
		if !ok {
			yield(service.StreamEvent{}, domain.ErrAlreadyStreaming)
			return
		}
		defer s.runs.end(msgID, active)

		started, err := s.deps.Live.Update(userID, convID, msgID, func(m domain.Message) (domain.Message, error) {
			if m.Status != domain.StatusPending {
				return m, domain.ErrInvalidState // another stream or a stop got there first
			}
			m.Status = domain.StatusStreaming
			return m, nil
		})
		if err != nil {
			yield(service.StreamEvent{}, err)
			return
		}
		s.run(runCtx, userID, started, yield)
	}
}

func (s *Service) run(ctx context.Context, userID uuid.UUID, started domain.Message, yield func(service.StreamEvent, error) bool) {
	start := time.Now()
	var acc accumulator
	var runErr error
	consumerGone, tooLong := false, false

	// A bug that panics while the reply is written ends the request, which the
	// transport recovers, but must not leave the reply streaming: the reply is
	// ended as interrupted, with the text that had arrived. Every other path
	// finishes it below.
	defer func() {
		if rec := recover(); rec != nil {
			reply := started
			reply.Content = acc.text()
			if _, err := s.endLost(userID, reply); err != nil {
				s.deps.Logger.ErrorContext(ctx, "ending a reply after a panic failed", "message_id", reply.ID, "error", err)
			}
			panic(rec)
		}
	}()

	events := s.deps.Agent.Run(ctx, domain.AgentRequest{UserID: userID, ConversationID: started.ConversationID, Text: started.Prompt})
	for ev, err := range events {
		if err != nil {
			runErr = err
			break
		}
		// Breaking out of the loop tells the agent adapter to cancel the upstream run.
		if acc.apply(ev, start) && !yield(service.StreamEvent{Kind: service.StreamSnapshot, Text: acc.text()}, nil) {
			consumerGone = true
			break
		}
		if acc.size() > s.deps.MaxReplyBytes {
			tooLong = true
			break
		}
	}

	content := acc.text()
	status, reason := s.outcome(ctx, started.ID, outcomeInput{consumerGone: consumerGone, tooLong: tooLong, err: runErr, content: content})
	meta := domain.Meta{
		TTFTMs:     ceilMillis(acc.ttft),
		DurationMs: ceilMillis(time.Since(start)),
		Usage:      acc.usage,
		ToolCalls:  acc.tools,
	}
	// A card is for paying or choosing on a reply that completed. On any other the agent's service has a transfer waiting that
	// the page would offer to pay on a reply that says it failed, so the card is not kept.
	if status == domain.StatusComplete {
		meta.Interaction = acc.card
	}

	_, err := s.deps.Live.Update(userID, started.ConversationID, started.ID, func(m domain.Message) (domain.Message, error) {
		if m.Status.Terminal() {
			return m, domain.ErrInvalidState // ended already (a Stop got there first): a finished reply is never overwritten
		}
		m.Content, m.Status, m.Error, m.Meta, m.Prompt = content, status, reason, meta, ""
		return m, nil
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// The server forgot the reply while it was being written.
		s.deps.Logger.InfoContext(ctx, "discarded a reply that was forgotten", "message_id", started.ID)
	case errors.Is(err, domain.ErrInvalidState):
		// It was ended in the meantime: whoever did it said how.
	case err != nil:
		s.deps.Logger.ErrorContext(ctx, "keeping a reply failed", "message_id", started.ID, "error", err)
	case !consumerGone:
		yield(doneEvent(status, content, reason, meta), nil)
	}
}

// outcomeInput is how a run ended, as far as the loop knows.
type outcomeInput struct {
	consumerGone bool   // the browser went away
	tooLong      bool   // the reply passed its limit and the run was stopped
	err          error  // what the agent said went wrong
	content      string // the text so far
}

// outcome decides how a run ended and what to tell the user. Details of
// failures are logged here.
//
// Pattern: Ordered Outcome Table
func (s *Service) outcome(ctx context.Context, msgID uuid.UUID, in outcomeInput) (domain.Status, string) {
	err, content := in.err, in.content
	fail := func(level func(context.Context, string, ...any), reason string) (domain.Status, string) {
		level(ctx, "reply failed", "message_id", msgID, "error", err)
		return domain.StatusFailed, reason
	}
	switch {
	case in.consumerGone || ctx.Err() != nil:
		return domain.StatusStopped, ""
	case in.tooLong:
		err = fmt.Errorf("the reply passed %d bytes", s.deps.MaxReplyBytes)
		return fail(s.deps.Logger.WarnContext, domain.FailureTooLong)
	case err == nil && content == "":
		err = domain.ErrIncompleteStream
		return fail(s.deps.Logger.WarnContext, domain.FailureIncomplete)
	case err == nil:
		return domain.StatusComplete, ""
	case errors.Is(err, context.DeadlineExceeded):
		return fail(s.deps.Logger.WarnContext, domain.FailureTimeout)
	case errors.Is(err, domain.ErrAgentUnavailable):
		return fail(s.deps.Logger.WarnContext, domain.FailureUnavailable)
	case errors.Is(err, domain.ErrIncompleteStream):
		return fail(s.deps.Logger.WarnContext, domain.FailureIncomplete)
	}
	if _, ok := errors.AsType[*domain.AgentError](err); ok {
		return fail(s.deps.Logger.ErrorContext, domain.FailureAgentError)
	}
	return fail(s.deps.Logger.ErrorContext, domain.FailureUnknown)
}

// ceilMillis rounds a time up to whole milliseconds, so a time longer than zero
// is at least 1, and 0 means "not measured".
func ceilMillis(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

func doneEvent(status domain.Status, content, reason string, meta domain.Meta) service.StreamEvent {
	return service.StreamEvent{Kind: service.StreamDone, Text: content, Status: status, Error: reason, Meta: meta}
}

// accumulator builds the reply from the agent's events. A reply can have
// several segments (text, a tool call, more text); each segment's final text
// replaces the deltas received for it.
type accumulator struct {
	segments []string
	finished int // bytes in segments
	current  strings.Builder
	tools    []string
	// card is the last card the agent put in the reply.
	card    *domain.Interaction
	usage   domain.Usage
	ttft    time.Duration
	hasText bool
}

// size is how many bytes of text the reply has so far.
func (a *accumulator) size() int { return a.finished + a.current.Len() }

// apply folds one event in and reports whether the visible text changed.
func (a *accumulator) apply(ev domain.AgentEvent, start time.Time) bool {
	switch ev.Kind {
	case domain.AgentDelta:
		a.markText(ev.Text, start)
		a.current.WriteString(ev.Text)
		return ev.Text != ""
	case domain.AgentFinal:
		a.markText(ev.Text, start)
		a.segments = append(a.segments, ev.Text)
		a.finished += len(ev.Text)
		a.current.Reset()
		return true
	case domain.AgentUsage:
		a.usage.Prompt += ev.Usage.Prompt
		a.usage.Completion += ev.Usage.Completion
	case domain.AgentToolCall:
		a.tools = append(a.tools, ev.Tool)
	case domain.AgentInteraction:
		// A card of no kind, or of one this version does not know, can be neither drawn nor answered: it is not kept, and it
		// does not take the place of one that was.
		if k := ev.Interaction.Kind; k == domain.InteractionApproval || k == domain.InteractionContact {
			a.card = &ev.Interaction
		}
	}
	return false
}

// markText records the time to the first piece of visible text.
func (a *accumulator) markText(text string, start time.Time) {
	if !a.hasText && text != "" {
		a.hasText = true
		a.ttft = time.Since(start)
	}
}

func (a *accumulator) text() string {
	parts := slices.Clone(a.segments)
	if a.current.Len() > 0 {
		parts = append(parts, a.current.String())
	}
	return strings.Join(parts, "\n\n")
}

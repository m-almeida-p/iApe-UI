package chat_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/chat"
	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/resource/mocks"
	"iape-ui/internal/core/port/service"
)

const maxChars = 50

var (
	ana  = uuid.MustParse("a0a0a0a0-0000-4000-8000-00000000000a")
	beto = uuid.MustParse("b0b0b0b0-0000-4000-8000-00000000000b")
)

// fakeLive is a LiveStore with what the tests need: it holds what it is given, for whom, and nothing else.
type fakeLive struct {
	mu      sync.Mutex
	owners  map[string]uuid.UUID
	replies map[uuid.UUID]domain.Message
	users   map[uuid.UUID]uuid.UUID
}

func newLive() *fakeLive {
	return &fakeLive{owners: map[string]uuid.UUID{}, replies: map[uuid.UUID]domain.Message{}, users: map[uuid.UUID]uuid.UUID{}}
}

var _ resource.LiveStore = (*fakeLive)(nil)

func (f *fakeLive) Adopt(u uuid.UUID, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.owners[id]; ok {
		return o == u
	}
	f.owners[id] = u
	return true
}

func (f *fakeLive) Owner(id string) (uuid.UUID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.owners[id]
	return u, ok
}

func (f *fakeLive) Begin(u uuid.UUID, m domain.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owners[m.ConversationID] != u {
		return domain.ErrNotFound
	}
	for id, r := range f.replies {
		if r.ConversationID == m.ConversationID && !r.Status.Terminal() {
			_ = id
			return domain.ErrInvalidState
		}
	}
	f.replies[m.ID], f.users[m.ID] = m, u
	return nil
}

func (f *fakeLive) Get(u uuid.UUID, conv string, id uuid.UUID) (domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.replies[id]
	if !ok || f.users[id] != u || m.ConversationID != conv {
		return domain.Message{}, domain.ErrNotFound
	}
	return m, nil
}

func (f *fakeLive) Update(u uuid.UUID, conv string, id uuid.UUID, change func(domain.Message) (domain.Message, error)) (domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.replies[id]
	if !ok || f.users[id] != u || m.ConversationID != conv {
		return domain.Message{}, domain.ErrNotFound
	}
	next, err := change(m)
	if err != nil {
		return m, err
	}
	f.replies[id] = next
	return next, nil
}

type rig struct {
	live    *fakeLive
	agent   *mocks.AgentGateway
	history *mocks.HistoryGateway
	logs    *bytes.Buffer
	svc     *chat.Service
}

func newRig(t *testing.T, mutate ...func(*chat.Dependencies)) *rig {
	t.Helper()
	r := &rig{live: newLive(), agent: mocks.NewAgentGateway(t), history: mocks.NewHistoryGateway(t), logs: &bytes.Buffer{}}
	deps := chat.Dependencies{
		Live: r.live, Agent: r.agent, History: r.history, MaxMessageChars: maxChars,
		Logger: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, m := range mutate {
		m(&deps)
	}
	r.svc = chat.NewService(deps)
	return r
}

// start makes a conversation of ana's and sends the first message: the turn, ready to stream.
func (r *rig) start(t *testing.T, content string) (string, service.Turn) {
	t.Helper()
	conv := r.svc.NewConversation(ana)
	turn, err := r.svc.Send(t.Context(), ana, conv, content)
	require.NoError(t, err)
	return conv, turn
}

func delta(text string) domain.AgentEvent {
	return domain.AgentEvent{Kind: domain.AgentDelta, Text: text}
}
func final(text string) domain.AgentEvent {
	return domain.AgentEvent{Kind: domain.AgentFinal, Text: text}
}

func events(evs ...domain.AgentEvent) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		for _, e := range evs {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func failing(after []domain.AgentEvent, err error) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		for _, e := range after {
			if !yield(e, nil) {
				return
			}
		}
		yield(domain.AgentEvent{}, err)
	}
}

// run expects the agent to be asked for the turn and to answer with seq.
func (r *rig) run(conv, prompt string, seq iter.Seq2[domain.AgentEvent, error]) {
	r.agent.On("Run", mock.Anything, domain.AgentRequest{UserID: ana, ConversationID: conv, Text: prompt}).Return(seq).Once()
}

func collect(seq iter.Seq2[service.StreamEvent, error]) ([]service.StreamEvent, error) {
	var out []service.StreamEvent
	for ev, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func lastOf(t *testing.T, evs []service.StreamEvent) service.StreamEvent {
	t.Helper()
	require.NotEmpty(t, evs)
	return evs[len(evs)-1]
}

// --- Conversations and Send -------------------------------------------------------------------------------------------------------

func TestNewConversation_IsAnIDOfTheAgentsFormAndOnlyTheUsers(t *testing.T) {
	r := newRig(t)

	id := r.svc.NewConversation(ana)
	other := r.svc.NewConversation(ana)

	_, err := domain.ParseConversationID(id)
	require.NoError(t, err, "the id the agent takes")
	assert.NotEqual(t, id, other)
	assert.True(t, r.svc.Owns(ana, id))
	assert.False(t, r.svc.Owns(beto, id))
	assert.False(t, r.svc.Owns(ana, "conv_v1_00000000-0000-4000-8000-000000000000"))
}

func TestSend_OfAConversationStartedHere(t *testing.T) {
	r := newRig(t)
	conv := r.svc.NewConversation(ana)

	turn, err := r.svc.Send(t.Context(), ana, conv, "  Hola  ")

	require.NoError(t, err)
	assert.Equal(t, domain.Message{ID: turn.User.ID, ConversationID: conv, Role: domain.RoleUser, Content: "Hola", Status: domain.StatusComplete}, turn.User)
	assert.Equal(t, domain.RoleAssistant, turn.Reply.Role)
	assert.Equal(t, domain.StatusPending, turn.Reply.Status)
	assert.Equal(t, "Hola", turn.Reply.Prompt)
	assert.NotEqual(t, turn.User.ID, turn.Reply.ID)
	got, err := r.svc.Reply(t.Context(), ana, conv, turn.Reply.ID)
	require.NoError(t, err)
	assert.Equal(t, turn.Reply, got)
}

func TestSend_RefusesWhatItShould(t *testing.T) {
	r := newRig(t)
	mine := r.svc.NewConversation(ana)
	valid := "conv_v1_11111111-1111-4111-8111-111111111111"

	tests := []struct {
		name    string
		user    uuid.UUID
		conv    string
		content string
		is      error
	}{
		{"an id that is not a conversation's", ana, "11111111-1111-4111-8111-111111111111", "hola", domain.ErrInvalidConversationID},
		{"an id in capitals", ana, strings.ToUpper(mine), "hola", domain.ErrInvalidConversationID},
		{"no id", ana, "", "hola", domain.ErrInvalidConversationID},
		{"an empty message", ana, mine, "   ", domain.ErrEmptyMessage},
		{"a message that is too long", ana, mine, strings.Repeat("a", maxChars+1), domain.ErrMessageTooLong},
		{"a message that cannot be kept", ana, mine, "a\x00b", domain.ErrMessageInvalid},
		{"the conversation of another user", beto, mine, "hola", domain.ErrNotFound},
		{"a conversation that nobody knows", ana, valid, "hola", domain.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.is == domain.ErrNotFound && tc.conv == valid {
				r.history.On("Messages", mock.Anything, tc.user, tc.conv, domain.PageRequest{Size: 1}).Return(domain.Transcript{}, domain.ErrNotFound).Once()
			}

			_, err := r.svc.Send(t.Context(), tc.user, tc.conv, tc.content)

			require.ErrorIs(t, err, tc.is)
		})
	}
}

func TestSend_ContinuesAConversationOfTheHistory(t *testing.T) {
	r := newRig(t)
	conv := "conv_v1_22222222-2222-4222-8222-222222222222"
	r.history.On("Messages", mock.Anything, ana, conv, domain.PageRequest{Size: 1}).Return(domain.Transcript{ConversationID: conv}, nil).Once()

	turn, err := r.svc.Send(t.Context(), ana, conv, "sigamos")

	require.NoError(t, err)
	assert.Equal(t, conv, turn.Reply.ConversationID)
	assert.True(t, r.svc.Owns(ana, conv), "the history said it is the user's: it is known now, and the history is not asked again")
	r.live.mu.Lock()
	r.live.replies = map[uuid.UUID]domain.Message{} // the turn is over
	r.live.mu.Unlock()
	_, err = r.svc.Send(t.Context(), ana, conv, "y otra vez")
	assert.NoError(t, err)
}

func TestSend_AConversationOfAnotherUserInTheHistoryIsNotWrittenTo(t *testing.T) {
	r := newRig(t)
	conv := "conv_v1_33333333-3333-4333-8333-333333333333"
	r.history.On("Messages", mock.Anything, beto, conv, mock.Anything).Return(domain.Transcript{}, domain.ErrNotFound).Once()

	_, err := r.svc.Send(t.Context(), beto, conv, "hola")

	assert.ErrorIs(t, err, domain.ErrNotFound, "the history answers for the account of the user: it is not theirs")
	assert.False(t, r.svc.Owns(beto, conv))
}

func TestSend_AHistoryThatCannotBeAskedIsNotAnAnswer(t *testing.T) {
	r := newRig(t)
	conv := "conv_v1_44444444-4444-4444-8444-444444444444"
	r.history.On("Messages", mock.Anything, ana, conv, mock.Anything).Return(domain.Transcript{}, fmt.Errorf("%w: down", domain.ErrHistoryUnavailable)).Once()

	_, err := r.svc.Send(t.Context(), ana, conv, "hola")

	require.ErrorIs(t, err, domain.ErrHistoryUnavailable)
	assert.NotErrorIs(t, err, domain.ErrNotFound, "the page says to try again, not that the chat is gone")
}

func TestSend_WithoutAHistoryOnlyWhatWasStartedHereIsWrittenTo(t *testing.T) {
	r := newRig(t, func(d *chat.Dependencies) { d.History = nil })
	conv := r.svc.NewConversation(ana)

	_, err := r.svc.Send(t.Context(), ana, conv, "hola")
	require.NoError(t, err)
	_, err = r.svc.Send(t.Context(), ana, "conv_v1_55555555-5555-4555-8555-555555555555", "hola")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestSend_OneTurnAtATime(t *testing.T) {
	r := newRig(t)
	conv, first := r.start(t, "uno")

	_, err := r.svc.Send(t.Context(), ana, conv, "dos")
	require.ErrorIs(t, err, domain.ErrInvalidState, "the first reply is not finished")

	r.run(conv, "uno", events(final("listo")))
	_, err = collect(r.svc.Stream(t.Context(), ana, conv, first.Reply.ID))
	require.NoError(t, err)
	_, err = r.svc.Send(t.Context(), ana, conv, "dos")
	assert.NoError(t, err)
}

func TestReply_IsTheUsersOnly(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")

	_, err := r.svc.Reply(t.Context(), beto, conv, turn.Reply.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.svc.Reply(t.Context(), ana, "conv_v1_00000000-0000-4000-8000-000000000000", turn.Reply.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// --- Stream -----------------------------------------------------------------------------------------------------------------------

func TestStream_AsksTheAgentForTheTurnInTheConversationAndKeepsTheReply(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "¿hora?")
	r.run(conv, "¿hora?", events(delta("Son "), delta("las 3"), final("Son las 3.")))

	evs, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	require.NoError(t, err)
	done := lastOf(t, evs)
	assert.Equal(t, service.StreamDone, done.Kind)
	assert.Equal(t, domain.StatusComplete, done.Status)
	assert.Equal(t, "Son las 3.", done.Text)
	for _, e := range evs[:len(evs)-1] {
		assert.Equal(t, service.StreamSnapshot, e.Kind)
	}
	kept, err := r.svc.Reply(t.Context(), ana, conv, turn.Reply.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusComplete, kept.Status)
	assert.Equal(t, "Son las 3.", kept.Content)
	assert.Empty(t, kept.Prompt, "the prompt is not kept once it was used")
	assert.Positive(t, kept.Meta.DurationMs)
}

func TestStream_ASegmentedReplyIsJoinedAndAToolCallIsNoted(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	r.run(conv, "hola", events(
		final("Uno."),
		domain.AgentEvent{Kind: domain.AgentToolCall, Tool: "balance"},
		domain.AgentEvent{Kind: domain.AgentUsage, Usage: domain.Usage{Prompt: 3, Completion: 4}},
		delta("Do"), final("Dos."),
	))

	evs, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	require.NoError(t, err)
	done := lastOf(t, evs)
	assert.Equal(t, "Uno.\n\nDos.", done.Text)
	assert.Equal(t, []string{"balance"}, done.Meta.ToolCalls)
	assert.Equal(t, domain.Usage{Prompt: 3, Completion: 4}, done.Meta.Usage)
}

func TestStream_ACardIsKeptOnACompleteReplyOnly(t *testing.T) {
	card := domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionPending, ApprovalID: "appr_1", RunID: "run-1"}
	tests := []struct {
		name string
		seq  func() iter.Seq2[domain.AgentEvent, error]
		want bool
	}{
		{"a reply that completed", func() iter.Seq2[domain.AgentEvent, error] {
			return events(final("Revisa."), domain.AgentEvent{Kind: domain.AgentInteraction, Interaction: card})
		}, true},
		{"a reply that failed", func() iter.Seq2[domain.AgentEvent, error] {
			return failing([]domain.AgentEvent{final("Revisa."), {Kind: domain.AgentInteraction, Interaction: card}}, domain.ErrAgentUnavailable)
		}, false},
		{"a card of no kind", func() iter.Seq2[domain.AgentEvent, error] {
			return events(final("Hola."), domain.AgentEvent{Kind: domain.AgentInteraction, Interaction: domain.Interaction{Kind: "mystery"}})
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			conv, turn := r.start(t, "paga")
			r.run(conv, "paga", tc.seq())

			evs, _ := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

			assert.Equal(t, tc.want, lastOf(t, evs).Meta.Interaction != nil)
		})
	}
}

func TestStream_HowARunFailsIsWhatTheUserIsTold(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		reason string
	}{
		{"the agent is down", fmt.Errorf("%w: refused", domain.ErrAgentUnavailable), domain.FailureUnavailable},
		{"it took too long", context.DeadlineExceeded, domain.FailureTimeout},
		{"the stream was cut", domain.ErrIncompleteStream, domain.FailureIncomplete},
		{"the agent reported an error", &domain.AgentError{Detail: "secret internals"}, domain.FailureAgentError},
		{"anything else", errors.New("boom"), domain.FailureUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			conv, turn := r.start(t, "hola")
			r.run(conv, "hola", failing([]domain.AgentEvent{delta("Par")}, tc.err))

			evs, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

			require.NoError(t, err)
			done := lastOf(t, evs)
			assert.Equal(t, domain.StatusFailed, done.Status)
			assert.Equal(t, tc.reason, done.Error)
			assert.Equal(t, "Par", done.Text, "what had arrived is kept")
			assert.NotContains(t, done.Error, "secret", "the user is not told the internals")
		})
	}
}

func TestStream_ARunThatSaysNothingIsIncomplete(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	r.run(conv, "hola", events())

	evs, _ := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	done := lastOf(t, evs)
	assert.Equal(t, domain.StatusFailed, done.Status)
	assert.Equal(t, domain.FailureIncomplete, done.Error)
}

func TestStream_AReplyThatGrowsTooLongIsCutOff(t *testing.T) {
	r := newRig(t, func(d *chat.Dependencies) { d.MaxReplyBytes = 10 })
	conv, turn := r.start(t, "hola")
	r.run(conv, "hola", events(delta("12345"), delta("67890"), delta("x"), delta("never")))

	evs, _ := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	done := lastOf(t, evs)
	assert.Equal(t, domain.StatusFailed, done.Status)
	assert.Equal(t, domain.FailureTooLong, done.Error)
	assert.NotContains(t, done.Text, "never")
}

func TestStream_AReplyThatIsFinishedIsReplayedWithoutTheAgent(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	r.run(conv, "hola", events(final("Hecho.")))
	_, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))
	require.NoError(t, err)

	evs, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID)) // the agent has no second Run expected

	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, service.StreamDone, evs[0].Kind)
	assert.Equal(t, "Hecho.", evs[0].Text)
}

func TestStream_IsForTheUsersOwnReplyOnly(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")

	_, err := collect(r.svc.Stream(t.Context(), beto, conv, turn.Reply.ID))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = collect(r.svc.Stream(t.Context(), ana, conv, uuid.New()))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestStream_ABrowserThatLeavesStopsTheAgentAndKeepsWhatCame(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	cancelled := make(chan struct{})
	r.agent.On("Run", mock.Anything, mock.Anything).Return(func(ctx context.Context, _ domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
		return func(yield func(domain.AgentEvent, error) bool) {
			if !yield(delta("Hel"), nil) {
				close(cancelled)
				return
			}
			<-ctx.Done()
			close(cancelled)
		}
	}).Once()

	for ev, err := range r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID) {
		require.NoError(t, err)
		if ev.Kind == service.StreamSnapshot {
			break // the browser leaves
		}
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent run was not stopped")
	}
	kept, err := r.svc.Reply(t.Context(), ana, conv, turn.Reply.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusStopped, kept.Status)
	assert.Equal(t, "Hel", kept.Content)
}

func TestStream_ATurnIsStreamedByOneConnectionAtATime(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	release := make(chan struct{})
	started := make(chan struct{})
	r.agent.On("Run", mock.Anything, mock.Anything).Return(func(ctx context.Context, _ domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
		return func(yield func(domain.AgentEvent, error) bool) {
			close(started)
			<-release
			yield(final("ok"), nil)
		}
	}).Once()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))
	}()
	<-started

	_, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	assert.ErrorIs(t, err, domain.ErrAlreadyStreaming)
	close(release)
	<-done
}

func TestStream_ARunThatPanicsDoesNotLeaveTheReplyStreaming(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	r.agent.On("Run", mock.Anything, mock.Anything).Return(func(context.Context, domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
		return func(yield func(domain.AgentEvent, error) bool) {
			yield(delta("Par"), nil)
			panic("bug")
		}
	}).Once()

	assert.PanicsWithValue(t, "bug", func() { _, _ = collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID)) })

	kept, err := r.svc.Reply(t.Context(), ana, conv, turn.Reply.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, kept.Status)
	assert.Equal(t, domain.FailureInterrupted, kept.Error)
	assert.Equal(t, "Par", kept.Content)
}

func TestStream_AReplyThatWasStreamingWithNoRunIsEnded(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	_, err := r.live.Update(ana, conv, turn.Reply.ID, func(m domain.Message) (domain.Message, error) {
		m.Status, m.Content = domain.StatusStreaming, "a medias"
		return m, nil
	})
	require.NoError(t, err)

	evs, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	require.NoError(t, err)
	done := lastOf(t, evs)
	assert.Equal(t, domain.StatusFailed, done.Status)
	assert.Equal(t, domain.FailureInterrupted, done.Error)
	assert.Equal(t, "a medias", done.Text)
	assert.Contains(t, r.logs.String(), "nothing was writing")
}

// --- Stop -------------------------------------------------------------------------------------------------------------------------

func TestStop_EndsARunAndReturnsWhatCame(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	gotText := make(chan struct{})
	r.agent.On("Run", mock.Anything, mock.Anything).Return(func(ctx context.Context, _ domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
		return func(yield func(domain.AgentEvent, error) bool) {
			if !yield(delta("Hel"), nil) {
				return
			}
			close(gotText)
			<-ctx.Done()
		}
	}).Once()
	streamed := make(chan []service.StreamEvent, 1)
	go func() {
		evs, _ := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))
		streamed <- evs
	}()
	<-gotText

	stopped, err := r.svc.Stop(t.Context(), ana, conv, turn.Reply.ID)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusStopped, stopped.Status)
	assert.Equal(t, "Hel", stopped.Content)
	evs := <-streamed
	done := lastOf(t, evs)
	assert.Equal(t, domain.StatusStopped, done.Status, "the stream that was running says it stopped")
}

func TestStop_OfAReplyNobodyStartedIsStoppedForGood(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")

	stopped, err := r.svc.Stop(t.Context(), ana, conv, turn.Reply.ID)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusStopped, stopped.Status)
	assert.Empty(t, stopped.Prompt)
	evs, err := collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID)) // no Run is expected: the agent is not asked
	require.NoError(t, err)
	assert.Equal(t, domain.StatusStopped, lastOf(t, evs).Status)
}

func TestStop_OfAFinishedReplyReturnsItAsItIs(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	r.run(conv, "hola", events(final("Hecho.")))
	_, _ = collect(r.svc.Stream(t.Context(), ana, conv, turn.Reply.ID))

	got, err := r.svc.Stop(t.Context(), ana, conv, turn.Reply.ID)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusComplete, got.Status)
	assert.Equal(t, "Hecho.", got.Content)
}

func TestStop_OfAReplyThatNothingWritesEndsIt(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")
	_, err := r.live.Update(ana, conv, turn.Reply.ID, func(m domain.Message) (domain.Message, error) {
		m.Status, m.Content = domain.StatusStreaming, "a medias"
		return m, nil
	})
	require.NoError(t, err)

	got, err := r.svc.Stop(t.Context(), ana, conv, turn.Reply.ID)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, got.Status)
	assert.Equal(t, domain.FailureInterrupted, got.Error)
}

func TestStop_IsForTheUsersOwnReplyOnly(t *testing.T) {
	r := newRig(t)
	conv, turn := r.start(t, "hola")

	_, err := r.svc.Stop(t.Context(), beto, conv, turn.Reply.ID)

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestNewService_HasDefaults(t *testing.T) {
	svc := chat.NewService(chat.Dependencies{Live: newLive(), MaxMessageChars: 10})
	conv := svc.NewConversation(ana)

	turn, err := svc.Send(t.Context(), ana, conv, "hola")

	require.NoError(t, err)
	assert.NotZero(t, turn.Reply.ID)
}

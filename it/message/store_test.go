// Integration tests for the message store. Group A: needs Docker.
package message_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/infrastructure/postgres"
	"iape-ui/it/internal/dbtest"
)

type fixture struct {
	pool          *pgxpool.Pool
	conversations *postgres.ConversationStore
	messages      *postgres.MessageStore
	alice, bob    uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	sql := dbtest.Statements(t)
	return &fixture{
		pool:          pool,
		conversations: postgres.NewConversationStore(pool, sql),
		messages:      postgres.NewMessageStore(pool, sql),
		alice:         dbtest.SeedUser(t, pool, "alice@example.com"),
		bob:           dbtest.SeedUser(t, pool, "bob@example.com"),
	}
}

func (f *fixture) conversation(t *testing.T, owner uuid.UUID) uuid.UUID {
	t.Helper()
	c, err := f.conversations.Create(t.Context(), domain.Conversation{ID: uuid.NewV7(), UserID: owner, Title: "chat"})
	require.NoError(t, err)
	return c.ID
}

// turn appends a pair and finishes the reply, so the next pair is allowed.
func (f *fixture) turn(t *testing.T, owner, conv uuid.UUID, prompt, answer string) (user, reply domain.Message) {
	t.Helper()
	user, reply, err := f.messages.AppendPair(t.Context(), owner, conv, prompt)
	require.NoError(t, err)
	_, err = f.messages.Claim(t.Context(), owner, conv, reply.ID)
	require.NoError(t, err)
	require.NoError(t, f.messages.Finish(t.Context(), reply.ID, resource.Outcome{Content: answer, Status: domain.StatusComplete}))
	return user, reply
}

func TestAppendPair(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("adds a complete user message and a pending reply with consecutive numbers", func(t *testing.T) {
		conv := f.conversation(t, f.alice)

		user, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "hello")

		require.NoError(t, err)
		assert.Equal(t, 1, user.Seq)
		assert.Equal(t, domain.RoleUser, user.Role)
		assert.Equal(t, domain.StatusComplete, user.Status)
		assert.Equal(t, "hello", user.Content)
		assert.Equal(t, 2, reply.Seq)
		assert.Equal(t, domain.RoleAssistant, reply.Role)
		assert.Equal(t, domain.StatusPending, reply.Status)
		assert.Empty(t, reply.Content)
		assert.Equal(t, conv, reply.ConversationID)
		assert.NotEqual(t, user.ID, reply.ID)

		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, []int{1, 2}, []int{got[0].Seq, got[1].Seq})
	})

	t.Run("continues the numbering and bumps the conversation's activity", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		f.turn(t, f.alice, conv, "one", "answer one")
		before, err := f.conversations.Get(ctx, f.alice, conv)
		require.NoError(t, err)

		user, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "two")

		require.NoError(t, err)
		assert.Equal(t, 3, user.Seq)
		assert.Equal(t, 4, reply.Seq)
		after, err := f.conversations.Get(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.True(t, after.UpdatedAt.After(before.UpdatedAt))
	})

	t.Run("refuses a new turn while the last reply is unfinished", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "first")
		require.NoError(t, err)

		_, _, err = f.messages.AppendPair(ctx, f.alice, conv, "second")
		assert.ErrorIs(t, err, domain.ErrInvalidState, "pending")

		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		_, _, err = f.messages.AppendPair(ctx, f.alice, conv, "second")
		assert.ErrorIs(t, err, domain.ErrInvalidState, "streaming")

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: domain.StatusStopped}))
		_, _, err = f.messages.AppendPair(ctx, f.alice, conv, "second")
		assert.NoError(t, err, "a stopped reply counts as finished")
	})

	t.Run("another user's conversation and unknown ones are not found", func(t *testing.T) {
		conv := f.conversation(t, f.alice)

		_, _, err := f.messages.AppendPair(ctx, f.bob, conv, "hi")
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, _, err = f.messages.AppendPair(ctx, f.alice, uuid.NewV7(), "hi")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("twenty concurrent appends: exactly one wins, numbering stays gap-free", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		var wins, conflicts atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				_, _, err := f.messages.AppendPair(ctx, f.alice, conv, "race")
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, domain.ErrInvalidState):
					conflicts.Add(1)
				}
			})
		}
		wg.Wait()

		assert.EqualValues(t, 1, wins.Load())
		assert.EqualValues(t, 19, conflicts.Load())
		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, 1, got[0].Seq)
		assert.Equal(t, 2, got[1].Seq)
	})

	t.Run("ten finished turns number 1 to 20 with no gaps", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		for range 10 {
			f.turn(t, f.alice, conv, "q", "a")
		}

		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		require.Len(t, got, 20)
		for i, m := range got {
			assert.Equal(t, i+1, m.Seq)
		}
	})
}

func TestClaim(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	conv := f.conversation(t, f.alice)
	user, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "what is 2+2?")
	require.NoError(t, err)

	t.Run("another user cannot claim it", func(t *testing.T) {
		_, err := f.messages.Claim(ctx, f.bob, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("the wrong conversation is not found", func(t *testing.T) {
		_, err := f.messages.Claim(ctx, f.alice, f.conversation(t, f.alice), reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("a user message is not claimable", func(t *testing.T) {
		_, err := f.messages.Claim(ctx, f.alice, conv, user.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("ten concurrent claims: one wins and gets the prompt, nine lose", func(t *testing.T) {
		var wins, losses atomic.Int32
		var winner resource.Claimed
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				c, err := f.messages.Claim(ctx, f.alice, conv, reply.ID)
				switch {
				case err == nil:
					wins.Add(1)
					winner = c
				case errors.Is(err, domain.ErrAlreadyStreaming):
					losses.Add(1)
				}
			})
		}
		wg.Wait()

		assert.EqualValues(t, 1, wins.Load())
		assert.EqualValues(t, 9, losses.Load())
		assert.Equal(t, "what is 2+2?", winner.Prompt)
		assert.Equal(t, domain.StatusStreaming, winner.Reply.Status)
		assert.Equal(t, reply.ID, winner.Reply.ID)
	})

	t.Run("a finished reply is an invalid state, not a streaming conflict", func(t *testing.T) {
		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Content: "4", Status: domain.StatusComplete}))

		_, err := f.messages.Claim(ctx, f.alice, conv, reply.ID)

		assert.ErrorIs(t, err, domain.ErrInvalidState)
	})
}

func TestFinish(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	newStreaming := func(t *testing.T) (conv uuid.UUID, reply domain.Message) {
		t.Helper()
		conv = f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
		require.NoError(t, err)
		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		return conv, reply
	}

	t.Run("records content, status and metadata", func(t *testing.T) {
		conv, reply := newStreaming(t)
		meta := domain.Meta{TTFTMs: 120, DurationMs: 3400, Usage: domain.Usage{Prompt: 10, Completion: 25}, ToolCalls: []string{"search"}}

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{
			Content: "# Answer\n\nforty-two", Status: domain.StatusComplete, Meta: meta,
		}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, "# Answer\n\nforty-two", got.Content)
		assert.Equal(t, domain.StatusComplete, got.Status)
		assert.Equal(t, meta, got.Meta)
		assert.Empty(t, got.Error)
	})

	t.Run("a stopped reply keeps its partial text", func(t *testing.T) {
		conv, reply := newStreaming(t)

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Content: "half an ans", Status: domain.StatusStopped}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusStopped, got.Status)
		assert.Equal(t, "half an ans", got.Content)
	})

	t.Run("a failed reply records why", func(t *testing.T) {
		conv, reply := newStreaming(t)

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: domain.StatusFailed, Error: "the agent is unavailable"}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusFailed, got.Status)
		assert.Equal(t, "the agent is unavailable", got.Error)
	})

	t.Run("empty metadata is stored as an empty object", func(t *testing.T) {
		_, reply := newStreaming(t)
		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: domain.StatusComplete}))

		var raw string
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT meta::text FROM messages WHERE id = $1`, reply.ID).Scan(&raw))
		assert.JSONEq(t, `{}`, raw)
	})

	t.Run("invalid transitions are refused", func(t *testing.T) {
		_, pending, err := f.messages.AppendPair(ctx, f.alice, f.conversation(t, f.alice), "q")
		require.NoError(t, err)
		_, done := newStreaming(t)
		require.NoError(t, f.messages.Finish(ctx, done.ID, resource.Outcome{Status: domain.StatusComplete}))
		_, streaming := newStreaming(t)

		tests := []struct {
			name string
			id   uuid.UUID
			out  resource.Outcome
			want error
		}{
			{"finishing a pending reply", pending.ID, resource.Outcome{Status: domain.StatusComplete}, domain.ErrInvalidState},
			{"finishing twice", done.ID, resource.Outcome{Status: domain.StatusFailed}, domain.ErrInvalidState},
			{"a non-terminal target status", streaming.ID, resource.Outcome{Status: domain.StatusPending}, domain.ErrInvalidState},
			{"an unknown status", streaming.ID, resource.Outcome{Status: "thinking"}, domain.ErrInvalidState},
			{"an unknown message", uuid.NewV7(), resource.Outcome{Status: domain.StatusComplete}, domain.ErrNotFound},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.ErrorIs(t, f.messages.Finish(ctx, tc.id, tc.out), tc.want)
			})
		}
	})
}

func TestGetAndList(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	conv := f.conversation(t, f.alice)
	user, reply := f.turn(t, f.alice, conv, "q", "a")

	t.Run("list returns the messages in order", func(t *testing.T) {
		got, err := f.messages.List(ctx, f.alice, conv)

		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, user.ID, got[0].ID)
		assert.Equal(t, reply.ID, got[1].ID)
		assert.Equal(t, "a", got[1].Content)
	})

	t.Run("ownership is enforced everywhere", func(t *testing.T) {
		tests := []struct {
			name string
			run  func() error
		}{
			{"list as another user", func() error { _, err := f.messages.List(ctx, f.bob, conv); return err }},
			{"list an unknown conversation", func() error { _, err := f.messages.List(ctx, f.alice, uuid.NewV7()); return err }},
			{"get as another user", func() error { _, err := f.messages.Get(ctx, f.bob, conv, reply.ID); return err }},
			{"get through the wrong conversation", func() error {
				_, err := f.messages.Get(ctx, f.alice, f.conversation(t, f.alice), reply.ID)
				return err
			}},
			{"get an unknown message", func() error { _, err := f.messages.Get(ctx, f.alice, conv, uuid.NewV7()); return err }},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.ErrorIs(t, tc.run(), domain.ErrNotFound)
			})
		}
	})

	t.Run("an empty conversation lists as empty, not missing", func(t *testing.T) {
		got, err := f.messages.List(ctx, f.alice, f.conversation(t, f.alice))

		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestReplaceLastAssistant(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("swaps a finished reply for a new pending one with the same number", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, old := f.turn(t, f.alice, conv, "q", "first answer")

		fresh, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, old.ID)

		require.NoError(t, err)
		assert.NotEqual(t, old.ID, fresh.ID)
		assert.Equal(t, old.Seq, fresh.Seq)
		assert.Equal(t, domain.StatusPending, fresh.Status)
		assert.Empty(t, fresh.Content)
		_, err = f.messages.Get(ctx, f.alice, conv, old.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, "the old reply is gone")
		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.Len(t, got, 2, "the user's message is untouched")

		claimed, err := f.messages.Claim(ctx, f.alice, conv, fresh.ID)
		require.NoError(t, err)
		assert.Equal(t, "q", claimed.Prompt, "the new reply answers the same question")
	})

	t.Run("a failed or stopped reply can be replaced too", func(t *testing.T) {
		for _, status := range []domain.Status{domain.StatusFailed, domain.StatusStopped} {
			conv := f.conversation(t, f.alice)
			_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
			require.NoError(t, err)
			_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
			require.NoError(t, err)
			require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: status}))

			_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)
			assert.NoError(t, err, string(status))
		}
	})

	t.Run("an unfinished reply is not replaceable", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
		require.NoError(t, err)

		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "pending")

		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "streaming")
	})

	t.Run("only the last reply can be replaced", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, first := f.turn(t, f.alice, conv, "q1", "a1")
		user2, second := f.turn(t, f.alice, conv, "q2", "a2")

		_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, first.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "an earlier reply")
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, user2.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "a user message is never replaced")

		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, second.ID)
		assert.NoError(t, err, "the last one is fine")
		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.Len(t, got, 4, "and nothing else changed")
		assert.Equal(t, first.ID, got[1].ID)
	})

	t.Run("a message that is not in the conversation is not found", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		f.turn(t, f.alice, conv, "q", "a")
		other := f.conversation(t, f.alice)
		_, elsewhere := f.turn(t, f.alice, other, "q", "a")

		_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, uuid.NewV7())
		assert.ErrorIs(t, err, domain.ErrNotFound, "unknown")
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, elsewhere.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, "belongs to another conversation")
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, f.conversation(t, f.alice), uuid.NewV7())
		assert.ErrorIs(t, err, domain.ErrNotFound, "an empty conversation")
	})

	t.Run("ownership is enforced", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply := f.turn(t, f.alice, conv, "q", "a")

		_, err := f.messages.ReplaceLastAssistant(ctx, f.bob, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, uuid.NewV7(), reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	// The old reply goes and the new one comes in one transaction. Between two
	// statements of their own, a reader would find the question alone.
	t.Run("a reader never sees the conversation without its last reply", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply := f.turn(t, f.alice, conv, "q", "a")
		const swaps, readers = 100, 4
		var (
			done    atomic.Bool
			reading sync.WaitGroup // released when every reader has looked once
			wg      sync.WaitGroup
			wrong   = make([]string, readers) // the first wrong view of each reader
		)
		reading.Add(readers)
		for i := range readers {
			wg.Go(func() {
				looked := sync.OnceFunc(reading.Done)
				for !done.Load() {
					got, err := f.messages.List(ctx, f.alice, conv)
					if wrong[i] == "" && (err != nil || len(got) != 2 || got[1].Role != domain.RoleAssistant) {
						wrong[i] = fmt.Sprintf("%d messages (error: %v)", len(got), err)
					}
					looked()
				}
			})
		}
		reading.Wait()

		swapAgainAndAgain := func() error {
			last := reply.ID
			for range swaps {
				fresh, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, last)
				if err != nil {
					return err
				}
				if _, err := f.messages.Claim(ctx, f.alice, conv, fresh.ID); err != nil {
					return err
				}
				if err := f.messages.Finish(ctx, fresh.ID, resource.Outcome{Content: "a", Status: domain.StatusComplete}); err != nil {
					return err
				}
				last = fresh.ID
			}
			return nil
		}
		err := swapAgainAndAgain()
		done.Store(true)
		wg.Wait()

		require.NoError(t, err)
		for _, view := range wrong {
			assert.Empty(t, view, "every view holds the question and one reply")
		}
	})
}

func TestRecoverInterrupted(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	streaming := func(t *testing.T) domain.Message {
		t.Helper()
		conv := f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
		require.NoError(t, err)
		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		return reply
	}
	age := func(t *testing.T, id uuid.UUID, by time.Duration) {
		t.Helper()
		_, err := f.pool.Exec(ctx, `UPDATE messages SET updated_at = now() - $2::interval WHERE id = $1`, id, by.String())
		require.NoError(t, err)
	}
	status := func(t *testing.T, id uuid.UUID) (domain.Status, string) {
		t.Helper()
		var s, e string
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT status, error FROM messages WHERE id = $1`, id).Scan(&s, &e))
		return domain.Status(s), e
	}

	stale, fresh := streaming(t), streaming(t)
	age(t, stale.ID, 10*time.Minute)
	_, pending, err := f.messages.AppendPair(ctx, f.alice, f.conversation(t, f.alice), "q")
	require.NoError(t, err)
	_, done := f.turn(t, f.alice, f.conversation(t, f.alice), "q", "a")

	n, err := f.messages.RecoverInterrupted(ctx, 5*time.Minute)

	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the stale one")
	s, e := status(t, stale.ID)
	assert.Equal(t, domain.StatusFailed, s)
	assert.Equal(t, domain.FailureInterrupted, e)
	s, _ = status(t, fresh.ID)
	assert.Equal(t, domain.StatusStreaming, s, "a recent reply may still be running")
	s, _ = status(t, pending.ID)
	assert.Equal(t, domain.StatusPending, s, "pending replies can still be run")
	s, _ = status(t, done.ID)
	assert.Equal(t, domain.StatusComplete, s)

	n, err = f.messages.RecoverInterrupted(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "at startup, with no grace, every remaining streaming reply is stale")
	s, _ = status(t, fresh.ID)
	assert.Equal(t, domain.StatusFailed, s)
}

// A call whose context has ended gives up with the context's error and changes
// nothing: the same call then does its work on a context that is alive.
func TestMessageStoreHonorsContext(t *testing.T) {
	f := newFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	// A conversation for each call to work on, in the state the call needs.
	empty := f.conversation(t, f.alice)
	read := f.conversation(t, f.alice)
	_, answered := f.turn(t, f.alice, read, "q", "a")
	waiting := f.conversation(t, f.alice)
	_, pending, err := f.messages.AppendPair(t.Context(), f.alice, waiting, "q")
	require.NoError(t, err)
	writing := f.conversation(t, f.alice)
	_, streaming, err := f.messages.AppendPair(t.Context(), f.alice, writing, "q")
	require.NoError(t, err)
	_, err = f.messages.Claim(t.Context(), f.alice, writing, streaming.ID)
	require.NoError(t, err)
	swapped := f.conversation(t, f.alice)
	_, finished := f.turn(t, f.alice, swapped, "q", "a")

	tests := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"AppendPair", func(ctx context.Context) error {
			_, _, err := f.messages.AppendPair(ctx, f.alice, empty, "q")
			return err
		}},
		{"List", func(ctx context.Context) error {
			_, err := f.messages.List(ctx, f.alice, read)
			return err
		}},
		{"Get", func(ctx context.Context) error {
			_, err := f.messages.Get(ctx, f.alice, read, answered.ID)
			return err
		}},
		{"Claim", func(ctx context.Context) error {
			_, err := f.messages.Claim(ctx, f.alice, waiting, pending.ID)
			return err
		}},
		{"Finish", func(ctx context.Context) error {
			return f.messages.Finish(ctx, streaming.ID, resource.Outcome{Status: domain.StatusComplete})
		}},
		{"ReplaceLastAssistant", func(ctx context.Context) error {
			_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, swapped, finished.ID)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.call(canceled), context.Canceled)
			require.NoError(t, tc.call(t.Context()), "the same call works on a context that is alive")
		})
	}
}

// cardReply is a finished reply that carries an approval card in the given state.
func (f *fixture) cardReply(t *testing.T, owner, conv uuid.UUID, state domain.InteractionState) domain.Message {
	t.Helper()
	_, reply, err := f.messages.AppendPair(t.Context(), owner, conv, "pay Rosa")
	require.NoError(t, err)
	_, err = f.messages.Claim(t.Context(), owner, conv, reply.ID)
	require.NoError(t, err)
	card := domain.Interaction{Kind: domain.InteractionApproval, State: state, ApprovalID: "appr_1", RunID: "run-1",
		Transfer: domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz"}}
	require.NoError(t, f.messages.Finish(t.Context(), reply.ID, resource.Outcome{
		Content: "Review your transfer.", Status: domain.StatusComplete,
		Meta: domain.Meta{TTFTMs: 120, ToolCalls: []string{"request_transfer_approval"}, Interaction: &card},
	}))
	return reply
}

func TestSaveInteraction(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("a card keeps its state, and the reply keeps the rest of its meta", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		before, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		paid, err := before.Meta.Interaction.Approved("Transfer paid successfully. TxnID: OP-1", "")
		require.NoError(t, err)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), paid))

		after, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		require.NotNil(t, after.Meta.Interaction)
		assert.Equal(t, paid, *after.Meta.Interaction)
		assert.Equal(t, int64(120), after.Meta.TTFTMs, "the other keys of meta are untouched")
		assert.Equal(t, []string{"request_transfer_approval"}, after.Meta.ToolCalls)
		assert.Equal(t, "Review your transfer.", after.Content)
	})

	t.Run("only one of two answers to the same state wins", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		card := domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionPaid, ApprovalID: "appr_1"}
		const callers = 8
		results := make(chan error, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Go(func() {
				results <- f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), card)
			})
		}
		wg.Wait()
		close(results)

		wins, refused := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, domain.ErrInvalidState):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		assert.Equal(t, 1, wins)
		assert.Equal(t, callers-1, refused)
	})

	t.Run("a card that is no longer in the state the caller saw is refused", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPaid)

		err := f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending),
			domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionRejected, ApprovalID: "appr_1"})

		require.ErrorIs(t, err, domain.ErrInvalidState)
		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State, "a refused save changes nothing")
	})

	t.Run("a reply with no card is refused", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply := f.turn(t, f.alice, conv, "hi", "hello")

		err := f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending),
			domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionPaid})

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("someone else's message and a missing one are not found", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		card := domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionRejected, ApprovalID: "appr_1"}

		require.ErrorIs(t, f.messages.SaveInteraction(ctx, f.bob, conv, reply.ID, at(domain.InteractionPending), card), domain.ErrNotFound)
		require.ErrorIs(t, f.messages.SaveInteraction(ctx, f.alice, conv, uuid.NewV7(), at(domain.InteractionPending), card), domain.ErrNotFound)
		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
	})
}

// replyIn is a reply of the given status that carries card in its meta, made the way the program makes one: appended, claimed,
// finished. A pending or a streaming reply cannot carry a card through Finish, so its meta is set in the table, as it would be by
// something that went wrong.
func (f *fixture) replyIn(t *testing.T, owner, conv uuid.UUID, status domain.Status, card domain.Interaction) domain.Message {
	t.Helper()
	ctx := t.Context()
	_, reply, err := f.messages.AppendPair(ctx, owner, conv, "pay Rosa")
	require.NoError(t, err)
	if status == domain.StatusPending {
		f.setMeta(t, reply.ID, fmt.Sprintf(`{"interaction": %s}`, mustJSON(t, card)))
		return reply
	}
	_, err = f.messages.Claim(ctx, owner, conv, reply.ID)
	require.NoError(t, err)
	if status == domain.StatusStreaming {
		f.setMeta(t, reply.ID, fmt.Sprintf(`{"interaction": %s}`, mustJSON(t, card)))
		return reply
	}
	require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{
		Content: "Review your transfer.", Status: status, Meta: domain.Meta{TTFTMs: 7, Interaction: &card},
	}))
	return reply
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

func (f *fixture) setMeta(t *testing.T, msgID uuid.UUID, meta string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `UPDATE messages SET meta = $2::jsonb WHERE id = $1`, msgID, meta)
	require.NoError(t, err)
}

// row is what the table holds of a message, as text, so that a test can say that nothing in it changed.
type row struct {
	Content, Status, Meta string
	UpdatedAt             time.Time
}

func (f *fixture) rowOf(t *testing.T, msgID uuid.UUID) row {
	t.Helper()
	var r row
	err := f.pool.QueryRow(t.Context(), `SELECT content, status, meta::text, updated_at FROM messages WHERE id = $1`, msgID).
		Scan(&r.Content, &r.Status, &r.Meta, &r.UpdatedAt)
	require.NoError(t, err)
	return r
}

func approval(state domain.InteractionState) domain.Interaction {
	return domain.Interaction{
		Kind: domain.InteractionApproval, State: state, ApprovalID: "appr_1", RunID: "run-1",
		Transfer: domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz"},
	}
}

func TestSaveInteraction_RepliesThatCannotBeAnswered(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	paid := approval(domain.InteractionPaid)

	for _, status := range []domain.Status{domain.StatusPending, domain.StatusStreaming, domain.StatusFailed, domain.StatusStopped} {
		t.Run("a reply that is "+string(status), func(t *testing.T) {
			conv := f.conversation(t, f.alice)
			reply := f.replyIn(t, f.alice, conv, status, approval(domain.InteractionPending))
			before := f.rowOf(t, reply.ID)

			err := f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), paid)

			require.ErrorIs(t, err, domain.ErrInvalidState)
			assert.Equal(t, before, f.rowOf(t, reply.ID), "nothing changed, not even updated_at")
		})
	}
	t.Run("a message of the user", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		user, _, err := f.messages.AppendPair(ctx, f.alice, conv, "pay Rosa")
		require.NoError(t, err)
		f.setMeta(t, user.ID, fmt.Sprintf(`{"interaction": %s}`, mustJSON(t, approval(domain.InteractionPending))))
		before := f.rowOf(t, user.ID)

		err = f.messages.SaveInteraction(ctx, f.alice, conv, user.ID, at(domain.InteractionPending), paid)

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Equal(t, before, f.rowOf(t, user.ID))
	})
	t.Run("a reply whose meta has no card", func(t *testing.T) {
		for name, meta := range map[string]string{
			"an empty object":      `{}`,
			"a null":               `null`,
			"a card that is null":  `{"interaction": null}`,
			"a card with no state": `{"interaction": {"kind": "approval"}}`,
			"other keys only":      `{"ttft_ms": 5, "tool_calls": ["x"]}`,
		} {
			conv := f.conversation(t, f.alice)
			_, reply := f.turn(t, f.alice, conv, "hi", "hello")
			f.setMeta(t, reply.ID, meta)
			before := f.rowOf(t, reply.ID)

			err := f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), paid)

			require.ErrorIs(t, err, domain.ErrInvalidState, name)
			assert.Equal(t, before, f.rowOf(t, reply.ID), name)
		}
	})
}

func TestSaveInteraction_TheCheckIsOnTheStateAlone(t *testing.T) {
	// The compare-and-set looks at the state and nothing else: it is the caller, the interaction service, that knows what kind of
	// card it read, and it saves what it made from that card. This is what the store does with a card of another kind.
	f := newFixture(t)
	ctx := t.Context()
	conv := f.conversation(t, f.alice)
	contact := domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionPending, RequestID: "req_1", Query: "Rosa"}
	reply := f.replyIn(t, f.alice, conv, domain.StatusComplete, contact)

	require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), approval(domain.InteractionPaid)))

	got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
	require.NoError(t, err)
	assert.Equal(t, approval(domain.InteractionPaid), *got.Meta.Interaction, "the card is replaced whole")
}

func TestSaveInteraction_ManyAtOnce(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("fifty callers from the same state: one wins", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		const callers = 50
		results := make(chan error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				card := approval(domain.InteractionPaid)
				card.Result = fmt.Sprint("answer ", i)
				<-start
				results <- f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), card)
			})
		}
		close(start)
		wg.Wait()
		close(results)

		wins, refused := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, domain.ErrInvalidState):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		assert.Equal(t, 1, wins)
		assert.Equal(t, callers-1, refused)
		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
		assert.True(t, strings.HasPrefix(got.Meta.Interaction.Result, "answer "), "the card is one caller's whole answer: %q", got.Meta.Interaction.Result)
	})
	t.Run("callers from different states, in turn", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		needsCode := approval(domain.InteractionOTPRequired)
		needsCode.ChallengeID = "chal_1"
		paid := approval(domain.InteractionPaid)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), needsCode))
		require.ErrorIs(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), paid), domain.ErrInvalidState,
			"the one who still saw it pending is late")
		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionOTPRequired), paid))
		require.ErrorIs(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionOTPRequired), paid), domain.ErrInvalidState,
			"and so is the one who saw it ask for the code")

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
	})
	t.Run("two cards of two replies do not meet", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		first := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		second := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, id := range []uuid.UUID{first.ID, second.ID} {
			wg.Go(func() {
				errs[i] = f.messages.SaveInteraction(ctx, f.alice, conv, id, at(domain.InteractionPending), approval(domain.InteractionRejected))
			})
		}
		wg.Wait()

		assert.NoError(t, errs[0])
		assert.NoError(t, errs[1], "each reply has its own card to answer")
	})
}

func TestSaveInteraction_WhatItWrites(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("text of every kind, a megabyte of it included", func(t *testing.T) {
		for name, result := range map[string]string{
			"quotes and backslashes": `say "hi" \ back\slash A \n`,
			"emoji and unicode":      "pagó 💸 日本語 שלום \u2028 \u200f",
			"new lines and tabs":     "line 1\nline 2\r\n\ttab",
			"json inside":            `{"a":[1,2,{"b":null}],"c":"</script>"}`,
			"sql inside":             `'; DROP TABLE messages; --`,
			"a megabyte":             strings.Repeat("ñ😀", 1<<18),
			"nothing":                "",
		} {
			conv := f.conversation(t, f.alice)
			reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
			card := approval(domain.InteractionPaid)
			card.Result = result
			card.Transfer.Message = result
			card.Prompt = result

			require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), card), name)

			got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
			require.NoError(t, err, name)
			assert.Equal(t, card, *got.Meta.Interaction, name)
		}
	})
	t.Run("a card with a character that jsonb cannot keep is an error, and changes nothing", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		before := f.rowOf(t, reply.ID)
		card := approval(domain.InteractionPaid)
		card.Result = "Transfer paid\x00 successfully"

		var err error
		require.NotPanics(t, func() {
			err = f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), card)
		})

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrInvalidState, "the card was not refused for its state: it could not be written")
		assert.NotErrorIs(t, err, domain.ErrNotFound)
		assert.ErrorContains(t, err, "saving the card")
		assert.Equal(t, before, f.rowOf(t, reply.ID))
		still, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPending, still.Meta.Interaction.State, "and the reply can still be answered")
	})
	t.Run("text that is not UTF-8 is an error before the database is asked", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		before := f.rowOf(t, reply.ID)
		card := approval(domain.InteractionPaid)
		card.Result = "bad \xff\xfe bytes"

		err := f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), card)

		require.ErrorContains(t, err, "encoding the card")
		assert.Equal(t, before, f.rowOf(t, reply.ID))
	})
	t.Run("an amount that is not a number is an error before the database is asked", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		before := f.rowOf(t, reply.ID)
		card := approval(domain.InteractionPaid)
		card.Transfer.Amount = math.NaN()

		err := f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), card)

		require.ErrorContains(t, err, "encoding the card")
		assert.Equal(t, before, f.rowOf(t, reply.ID))
	})
	t.Run("a state to compare with that holds a NUL is an error, and changes nothing", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		before := f.rowOf(t, reply.ID)

		var err error
		require.NotPanics(t, func() {
			err = f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at("pend\x00ing"), approval(domain.InteractionPaid))
		})

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound)
		assert.Equal(t, before, f.rowOf(t, reply.ID))
	})
	t.Run("a card with only its zero values", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)

		// Its state is empty, so nobody can answer it again: from "pending" is not "".
		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), domain.Interaction{}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.Interaction{}, *got.Meta.Interaction)
		assert.ErrorIs(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), approval(domain.InteractionPaid)), domain.ErrInvalidState)
	})
	t.Run("the rest of the reply is as it was", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		// A newer program wrote keys this one does not know, among the ones it does.
		f.setMeta(t, reply.ID, fmt.Sprintf(`{
			"ttft_ms": 120, "duration_ms": 4500, "usage": {"prompt": 11, "completion": 22, "cached": 3},
			"tool_calls": ["request_transfer_approval", "list_contacts"],
			"future_key": {"nested": [1, 2.50, {"deep": null}], "text": "ñ 😀 \"q\""}, "another": false,
			"interaction": %s}`, mustJSON(t, approval(domain.InteractionPending))))
		others := func() string {
			var s string
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT (meta - 'interaction')::text FROM messages WHERE id = $1`, reply.ID).Scan(&s))
			return s
		}
		keysBefore, before := others(), f.rowOf(t, reply.ID)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), approval(domain.InteractionPaid)))

		assert.Equal(t, keysBefore, others(), "every other key is as it was, the unknown ones too")
		after := f.rowOf(t, reply.ID)
		assert.Equal(t, before.Content, after.Content)
		assert.Equal(t, before.Status, after.Status)
		assert.True(t, after.UpdatedAt.After(before.UpdatedAt), "updated_at moved: %s -> %s", before.UpdatedAt, after.UpdatedAt)
		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(120), got.Meta.TTFTMs)
		assert.Equal(t, int64(4500), got.Meta.DurationMs)
		assert.Equal(t, []string{"request_transfer_approval", "list_contacts"}, got.Meta.ToolCalls)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
	})
	t.Run("the conversation's activity and the other messages are not touched", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, other := f.turn(t, f.alice, conv, "hi", "hello")
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		beforeOther := f.rowOf(t, other.ID)
		beforeConv, err := f.conversations.Get(ctx, f.alice, conv)
		require.NoError(t, err)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), approval(domain.InteractionPaid)))

		assert.Equal(t, beforeOther, f.rowOf(t, other.ID))
		afterConv, err := f.conversations.Get(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.Equal(t, beforeConv, afterConv)
	})
}

func TestSaveInteraction_WhoseMessageItIs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	paid := approval(domain.InteractionPaid)

	t.Run("the conversation was deleted after the reply was read", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		_, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		require.NoError(t, f.conversations.Delete(ctx, f.alice, conv))

		err = f.messages.SaveInteraction(ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), paid)

		require.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("the conversation of someone else, with the right ids", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		before := f.rowOf(t, reply.ID)

		err := f.messages.SaveInteraction(ctx, f.bob, conv, reply.ID, at(domain.InteractionPending), paid)

		require.ErrorIs(t, err, domain.ErrNotFound)
		assert.Equal(t, before, f.rowOf(t, reply.ID))
	})
	t.Run("a conversation of the user's own that is not the message's", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		elsewhere := f.conversation(t, f.alice)
		before := f.rowOf(t, reply.ID)

		err := f.messages.SaveInteraction(ctx, f.alice, elsewhere, reply.ID, at(domain.InteractionPending), paid)

		require.ErrorIs(t, err, domain.ErrNotFound)
		assert.Equal(t, before, f.rowOf(t, reply.ID))
	})
	t.Run("a conversation of someone else that is not the message's", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		bobs := f.conversation(t, f.bob)

		err := f.messages.SaveInteraction(ctx, f.bob, bobs, reply.ID, at(domain.InteractionPending), paid)

		require.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("ids that are not there", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)

		assert.ErrorIs(t, f.messages.SaveInteraction(ctx, f.alice, uuid.NewV7(), reply.ID, at(domain.InteractionPending), paid), domain.ErrNotFound)
		assert.ErrorIs(t, f.messages.SaveInteraction(ctx, uuid.NewV7(), conv, reply.ID, at(domain.InteractionPending), paid), domain.ErrNotFound)
		assert.ErrorIs(t, f.messages.SaveInteraction(ctx, f.alice, conv, uuid.Nil(), at(domain.InteractionPending), paid), domain.ErrNotFound)
	})
}

func TestSaveInteraction_AContextThatEnds(t *testing.T) {
	f := newFixture(t)
	conv := f.conversation(t, f.alice)
	reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
	before := f.rowOf(t, reply.ID)
	past, cancelPast := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelPast()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	for name, tc := range map[string]struct {
		ctx  context.Context
		want error
	}{
		"canceled":          {canceled, context.Canceled},
		"a deadline passed": {past, context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			err := f.messages.SaveInteraction(tc.ctx, f.alice, conv, reply.ID, at(domain.InteractionPending), approval(domain.InteractionPaid))

			require.ErrorIs(t, err, tc.want)
			assert.NotErrorIs(t, err, domain.ErrInvalidState, "a card is not refused because the caller gave up")
			assert.NotErrorIs(t, err, domain.ErrNotFound)
			assert.Equal(t, before, f.rowOf(t, reply.ID))
		})
	}
	t.Run("the same call on a live context works", func(t *testing.T) {
		require.NoError(t, f.messages.SaveInteraction(t.Context(), f.alice, conv, reply.ID, at(domain.InteractionPending), approval(domain.InteractionPaid)))
	})
}

func TestFinish_StoresACardAndGetReadsItBack(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tests := map[string]domain.Interaction{
		"an approval with a transfer":      approval(domain.InteractionPending),
		"an approval with a zero transfer": {Kind: domain.InteractionApproval, State: domain.InteractionPending, ApprovalID: "appr_2"},
		"a card that asks for a code":      {Kind: domain.InteractionApproval, State: domain.InteractionOTPRequired, ApprovalID: "appr_3", ChallengeID: "chal_3", Transfer: domain.Transfer{Amount: 0.1}},
		"a contact card":                   {Kind: domain.InteractionContact, State: domain.InteractionPending, RequestID: "req_1", Query: "Rosa 😀", Prompt: "Who do you mean?"},
		"a contact card that was answered": {Kind: domain.InteractionContact, State: domain.InteractionConfirmed, RequestID: "req_2", Contact: domain.Contact{Name: "Rosa Díaz", Phone: "999888777"}},
		"a card of text that needs escaping": {Kind: domain.InteractionApproval, State: domain.InteractionPaid, ApprovalID: "a\"b\\c", Result: "pagó \"S/ 20\"\n💸",
			Transfer: domain.Transfer{DestinationName: "O'Brien <x>", Message: "line 1\nline 2"}},
	}
	for name, card := range tests {
		t.Run(name, func(t *testing.T) {
			conv := f.conversation(t, f.alice)
			_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "pay Rosa")
			require.NoError(t, err)
			_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
			require.NoError(t, err)

			require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{
				Content: "Review.", Status: domain.StatusComplete,
				Meta: domain.Meta{TTFTMs: 5, DurationMs: 90, ToolCalls: []string{"t"}, Interaction: &card},
			}))

			got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
			require.NoError(t, err)
			require.NotNil(t, got.Meta.Interaction)
			assert.Equal(t, card, *got.Meta.Interaction)
			assert.Equal(t, int64(5), got.Meta.TTFTMs)
			listed, err := f.messages.List(ctx, f.alice, conv)
			require.NoError(t, err)
			require.NotNil(t, listed[1].Meta.Interaction, "and the list has it as well")
			assert.Equal(t, card, *listed[1].Meta.Interaction)
		})
	}
	t.Run("a reply that finishes with no card has none, and the table has no key for it", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply := f.turn(t, f.alice, conv, "hi", "hello")

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)

		require.NoError(t, err)
		assert.Nil(t, got.Meta.Interaction)
		var hasKey bool
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT meta ? 'interaction' FROM messages WHERE id = $1`, reply.ID).Scan(&hasKey))
		assert.False(t, hasKey)
	})
}

// at is a card as it is read in a state, under no claim: what a caller that read it passes to SaveInteraction as from.
func at(state domain.InteractionState) domain.Interaction { return domain.Interaction{State: state} }

func TestSaveInteraction_TheClaimOfACard(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	newCard := func(t *testing.T) (uuid.UUID, uuid.UUID, domain.Interaction) {
		t.Helper()
		conv := f.conversation(t, f.alice)
		reply := f.cardReply(t, f.alice, conv, domain.InteractionPending)
		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		return conv, reply.ID, *got.Meta.Interaction
	}
	read := func(t *testing.T, conv, id uuid.UUID) domain.Interaction {
		t.Helper()
		got, err := f.messages.Get(ctx, f.alice, conv, id)
		require.NoError(t, err)
		require.NotNil(t, got.Meta.Interaction)
		return *got.Meta.Interaction
	}

	t.Run("a card is claimed, and finished by the one that holds the claim", func(t *testing.T) {
		conv, id, card := newCard(t)
		claimed, err := card.Claim("claim-1", now)
		require.NoError(t, err)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, card, claimed))
		assert.Equal(t, claimed, read(t, conv, id), "the claim is saved with its id, its time and the state to go back to")

		paid, err := card.Approved("Paid", "")
		require.NoError(t, err)
		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, claimed, paid))
		assert.Equal(t, paid, read(t, conv, id))
		assert.Empty(t, read(t, conv, id).ClaimID, "a finished card carries no claim")
	})

	t.Run("a claim is given back, and the card is as it was", func(t *testing.T) {
		conv, id, card := newCard(t)
		claimed, err := card.Claim("claim-1", now)
		require.NoError(t, err)
		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, card, claimed))

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, claimed, claimed.Base()))

		assert.Equal(t, card, read(t, conv, id))
	})

	t.Run("the claim of another cannot finish a card", func(t *testing.T) {
		conv, id, card := newCard(t)
		claimed, err := card.Claim("claim-1", now)
		require.NoError(t, err)
		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, card, claimed))
		paid, err := card.Approved("Paid", "")
		require.NoError(t, err)
		other := claimed
		other.ClaimID = "claim-2"

		err = f.messages.SaveInteraction(ctx, f.alice, conv, id, other, paid)

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Equal(t, claimed, read(t, conv, id), "nothing changed")
	})

	t.Run("a card that is claimed is not claimed again by one that read it before", func(t *testing.T) {
		conv, id, card := newCard(t)
		first, err := card.Claim("claim-1", now)
		require.NoError(t, err)
		second, err := card.Claim("claim-2", now)
		require.NoError(t, err)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, card, first))
		err = f.messages.SaveInteraction(ctx, f.alice, conv, id, card, second)

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Equal(t, "claim-1", read(t, conv, id).ClaimID)
	})

	t.Run("a stale claim is taken over by one, and the old holder loses", func(t *testing.T) {
		conv, id, card := newCard(t)
		old, err := card.Claim("claim-1", now)
		require.NoError(t, err)
		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, card, old))
		later := now.Add(domain.ClaimTTL)
		taken, err := old.Reclaim("claim-2", later)
		require.NoError(t, err)

		require.NoError(t, f.messages.SaveInteraction(ctx, f.alice, conv, id, old, taken))

		assert.Equal(t, "claim-2", read(t, conv, id).ClaimID)
		paid, err := card.Approved("Paid", "")
		require.NoError(t, err)
		err = f.messages.SaveInteraction(ctx, f.alice, conv, id, old, paid)
		require.ErrorIs(t, err, domain.ErrInvalidState, "the holder of the old claim is too late to finish it")
		assert.Equal(t, "claim-2", read(t, conv, id).ClaimID)
	})

	t.Run("of fifty that claim the same card exactly one holds it", func(t *testing.T) {
		conv, id, card := newCard(t)
		const callers = 50
		results := make(chan error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				claimed, err := card.Claim(fmt.Sprintf("claim-%d", i), now)
				if err != nil {
					results <- err
					return
				}
				results <- f.messages.SaveInteraction(ctx, f.alice, conv, id, card, claimed)
			})
		}
		wg.Wait()
		close(results)

		wins, refused := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, domain.ErrInvalidState):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		assert.Equal(t, 1, wins)
		assert.Equal(t, callers-1, refused)
		assert.Equal(t, domain.InteractionProcessing, read(t, conv, id).State)
	})
}

// The reply that holds a card whose payment the service may have taken is not replaced: the new turn could ask for the same
// transfer again, and the answer on its way would find the reply gone.
func TestReplaceLastAssistant_NotTheReplyThatHoldsAPayment(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	for _, state := range []domain.InteractionState{domain.InteractionProcessing, domain.InteractionOTPRequired, domain.InteractionPaid} {
		t.Run("a card that is "+string(state), func(t *testing.T) {
			conv := f.conversation(t, f.alice)
			reply := f.cardReply(t, f.alice, conv, state)

			_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)

			require.ErrorIs(t, err, domain.ErrInvalidState)
			got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
			require.NoError(t, err, "the reply is still there")
			assert.Equal(t, state, got.Meta.Interaction.State)
		})
	}

	for _, state := range []domain.InteractionState{domain.InteractionPending, domain.InteractionRejected} {
		t.Run("a card that is "+string(state)+" can be asked again", func(t *testing.T) {
			conv := f.conversation(t, f.alice)
			reply := f.cardReply(t, f.alice, conv, state)

			fresh, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)

			require.NoError(t, err)
			assert.NotEqual(t, reply.ID, fresh.ID)
		})
	}
}

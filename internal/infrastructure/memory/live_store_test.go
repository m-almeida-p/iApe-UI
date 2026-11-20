package memory_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/memory"
)

const conv = "conv_v1_11111111-1111-4111-8111-000000000001"

var ana, beto = uuid.New(), uuid.New()

// clock is a time that moves when the test says.
type clock struct{ t atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.t.Store(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func reply(conversation string, status domain.Status) domain.Message {
	return domain.Message{ID: uuid.New(), ConversationID: conversation, Role: domain.RoleAssistant, Status: status}
}

func TestAdopt_TheFirstUserKeepsTheConversation(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})

	assert.True(t, s.Adopt(ana, conv))
	assert.True(t, s.Adopt(ana, conv), "again is fine")
	assert.False(t, s.Adopt(beto, conv), "another user does not take it over")
	owner, known := s.Owner(conv)
	assert.True(t, known)
	assert.Equal(t, ana, owner)
	_, known = s.Owner("conv_v1_other")
	assert.False(t, known)
}

func TestBegin_NeedsAConversationOfTheUser(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})
	require.True(t, s.Adopt(ana, conv))

	assert.ErrorIs(t, s.Begin(beto, reply(conv, domain.StatusPending)), domain.ErrNotFound, "the conversation is ana's")
	assert.ErrorIs(t, s.Begin(ana, reply("conv_v1_nobody", domain.StatusPending)), domain.ErrNotFound, "no one adopted that one")
	assert.NoError(t, s.Begin(ana, reply(conv, domain.StatusPending)))
}

func TestBegin_OneReplyAtATimeInAConversation(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})
	require.True(t, s.Adopt(ana, conv))
	first := reply(conv, domain.StatusPending)
	require.NoError(t, s.Begin(ana, first))

	assert.ErrorIs(t, s.Begin(ana, reply(conv, domain.StatusPending)), domain.ErrInvalidState, "the first is not finished")

	_, err := s.Update(ana, conv, first.ID, func(m domain.Message) (domain.Message, error) { m.Status = domain.StatusComplete; return m, nil })
	require.NoError(t, err)
	assert.NoError(t, s.Begin(ana, reply(conv, domain.StatusPending)), "it finished: the next can start")
}

func TestBegin_AReplyNobodyStartedDoesNotBlockForLong(t *testing.T) {
	c := newClock()
	s := memory.NewLiveStore(memory.Options{Now: c.now})
	require.True(t, s.Adopt(ana, conv))
	require.NoError(t, s.Begin(ana, reply(conv, domain.StatusPending)))

	c.advance(20 * time.Second) // its stream is still to open
	assert.ErrorIs(t, s.Begin(ana, reply(conv, domain.StatusPending)), domain.ErrInvalidState)
	c.advance(20 * time.Second) // a page opens it at once: this one was lost, and the user can write again
	assert.NoError(t, s.Begin(ana, reply(conv, domain.StatusPending)))
}

func TestBegin_AReplyBeingWrittenBlocksUntilItsTime(t *testing.T) {
	c := newClock()
	s := memory.NewLiveStore(memory.Options{Now: c.now})
	require.True(t, s.Adopt(ana, conv))
	require.NoError(t, s.Begin(ana, reply(conv, domain.StatusStreaming)))

	c.advance(14 * time.Minute)
	assert.ErrorIs(t, s.Begin(ana, reply(conv, domain.StatusPending)), domain.ErrInvalidState)
	c.advance(2 * time.Minute)
	assert.NoError(t, s.Begin(ana, reply(conv, domain.StatusPending)))
}

func TestGet_AReplyIsItsUsers(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusPending)
	r.Content = "hola"
	require.NoError(t, s.Begin(ana, r))

	got, err := s.Get(ana, conv, r.ID)
	require.NoError(t, err)
	assert.Equal(t, "hola", got.Content)

	_, err = s.Get(beto, conv, r.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "another user's reply is not found")
	_, err = s.Get(ana, "conv_v1_other", r.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "nor is it in another conversation")
	_, err = s.Get(ana, conv, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestUpdate_ChangesInOneStepAndKeepsWhereItIs(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusPending)
	require.NoError(t, s.Begin(ana, r))

	got, err := s.Update(ana, conv, r.ID, func(m domain.Message) (domain.Message, error) {
		m.Content, m.ID, m.ConversationID = "hecho", uuid.New(), "conv_v1_elsewhere" // what must not move
		return m, nil
	})

	require.NoError(t, err)
	assert.Equal(t, "hecho", got.Content)
	assert.Equal(t, r.ID, got.ID)
	assert.Equal(t, conv, got.ConversationID)
	_, err = s.Update(beto, conv, r.ID, func(m domain.Message) (domain.Message, error) { return m, nil })
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestUpdate_AnErrorLeavesTheReplyAsItWas(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusPending)
	r.Content = "antes"
	require.NoError(t, s.Begin(ana, r))
	boom := errors.New("no")

	_, err := s.Update(ana, conv, r.ID, func(m domain.Message) (domain.Message, error) { m.Content = "después"; return m, boom })

	require.ErrorIs(t, err, boom)
	got, _ := s.Get(ana, conv, r.ID)
	assert.Equal(t, "antes", got.Content)
}

func TestUpdate_ChangesDoNotRaceAndNoneIsLost(t *testing.T) {
	s := memory.NewLiveStore(memory.Options{})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusComplete)
	require.NoError(t, s.Begin(ana, r))
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			_, err := s.Update(ana, conv, r.ID, func(m domain.Message) (domain.Message, error) { m.Meta.TTFTMs++; return m, nil })
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	got, err := s.Get(ana, conv, r.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 200, got.Meta.TTFTMs)
}

func TestTheStoreForgetsAfterItsTTL(t *testing.T) {
	c := newClock()
	s := memory.NewLiveStore(memory.Options{TTL: time.Hour, Now: c.now})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusComplete)
	require.NoError(t, s.Begin(ana, r))

	c.advance(59 * time.Minute)
	_, err := s.Get(ana, conv, r.ID)
	require.NoError(t, err)
	c.advance(2 * time.Minute) // a read in between did not extend it: 61 minutes since it was written
	_, err = s.Get(ana, conv, r.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, known := s.Owner(conv)
	assert.False(t, known, "the conversation is forgotten too, and can be adopted by anyone")
	assert.True(t, s.Adopt(beto, conv))
}

func TestUpdate_KeepsTheReplyAlive(t *testing.T) {
	c := newClock()
	s := memory.NewLiveStore(memory.Options{TTL: time.Hour, Now: c.now})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusComplete)
	require.NoError(t, s.Begin(ana, r))

	c.advance(50 * time.Minute)
	_, err := s.Update(ana, conv, r.ID, func(m domain.Message) (domain.Message, error) { return m, nil })
	require.NoError(t, err)
	c.advance(50 * time.Minute)

	_, err = s.Get(ana, conv, r.ID)
	assert.NoError(t, err)
}

func TestTheStoreIsBounded(t *testing.T) {
	t.Run("a full store drops its oldest finished reply for a new one", func(t *testing.T) {
		c := newClock()
		s := memory.NewLiveStore(memory.Options{MaxReplies: 2, Now: c.now})
		var first domain.Message
		for i := range 3 {
			id := "conv_v1_00000000-0000-4000-8000-00000000000" + string(rune('1'+i))
			require.True(t, s.Adopt(ana, id))
			r := reply(id, domain.StatusComplete)
			require.NoError(t, s.Begin(ana, r))
			if i == 0 {
				first = r
			}
			c.advance(time.Second)
		}

		_, err := s.Get(ana, first.ConversationID, first.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, "the oldest went")
	})

	t.Run("a store full of unfinished replies is busy", func(t *testing.T) {
		s := memory.NewLiveStore(memory.Options{MaxReplies: 1})
		require.True(t, s.Adopt(ana, conv))
		require.NoError(t, s.Begin(ana, reply(conv, domain.StatusStreaming)))
		other := "conv_v1_00000000-0000-4000-8000-000000000009"
		require.True(t, s.Adopt(beto, other))

		assert.ErrorIs(t, s.Begin(beto, reply(other, domain.StatusPending)), domain.ErrBusy)
	})

	t.Run("a full set of conversations forgets the oldest", func(t *testing.T) {
		c := newClock()
		s := memory.NewLiveStore(memory.Options{MaxConversations: 2, Now: c.now})
		require.True(t, s.Adopt(ana, "conv_a"))
		c.advance(time.Second)
		require.True(t, s.Adopt(ana, "conv_b"))
		c.advance(time.Second)
		require.True(t, s.Adopt(ana, "conv_c"))

		_, known := s.Owner("conv_a")
		assert.False(t, known)
		_, known = s.Owner("conv_c")
		assert.True(t, known)
	})
}

// A lookup does not wait for the sweep, which runs once a minute at most: what has expired is gone for whoever asks, before it is
// forgotten for good.
func TestAnExpiredEntryIsGoneBeforeTheSweepForgetsIt(t *testing.T) {
	c := newClock()
	s := memory.NewLiveStore(memory.Options{TTL: 10 * time.Second, Now: c.now})
	require.True(t, s.Adopt(ana, conv)) // the sweep runs here
	r := reply(conv, domain.StatusComplete)
	require.NoError(t, s.Begin(ana, r))

	c.advance(30 * time.Second) // less than the minute between sweeps, more than the TTL

	_, err := s.Get(ana, conv, r.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.Update(ana, conv, r.ID, func(m domain.Message) (domain.Message, error) { return m, nil })
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, known := s.Owner(conv)
	assert.False(t, known)
	assert.ErrorIs(t, s.Begin(ana, reply(conv, domain.StatusPending)), domain.ErrNotFound, "the conversation expired too")
	assert.True(t, s.Adopt(beto, conv), "and can be adopted by anyone")
}

func conversationID(n int) string { return fmt.Sprintf("conv_v1_00000000-0000-4000-8000-%012d", n) }

// One user cannot use up the store for the others: past their own limit they lose their own oldest.
func TestAUserPastTheirLimitLosesTheirOwnOldest(t *testing.T) {
	t.Run("conversations", func(t *testing.T) {
		c := newClock()
		s := memory.NewLiveStore(memory.Options{MaxConversationsPerUser: 3, Now: c.now})
		require.True(t, s.Adopt(beto, "conv_of_beto")) // the oldest of all
		c.advance(time.Second)
		for i := range 5 {
			require.True(t, s.Adopt(ana, conversationID(i)))
			c.advance(time.Second)
		}

		_, known := s.Owner("conv_of_beto")
		assert.True(t, known, "another user's conversation is not touched, old as it is")
		_, known = s.Owner(conversationID(0))
		assert.False(t, known, "ana's own oldest went")
		_, known = s.Owner(conversationID(4))
		assert.True(t, known)
	})

	t.Run("replies", func(t *testing.T) {
		c := newClock()
		s := memory.NewLiveStore(memory.Options{MaxRepliesPerUser: 2, Now: c.now})
		other := reply(conv, domain.StatusComplete)
		require.True(t, s.Adopt(beto, conv))
		require.NoError(t, s.Begin(beto, other))
		var first domain.Message
		for i := range 4 {
			id := conversationID(i)
			require.True(t, s.Adopt(ana, id))
			r := reply(id, domain.StatusComplete)
			require.NoError(t, s.Begin(ana, r))
			if i == 0 {
				first = r
			}
			c.advance(time.Second)
		}

		_, err := s.Get(ana, first.ConversationID, first.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, "ana's oldest finished reply went")
		_, err = s.Get(beto, conv, other.ID)
		assert.NoError(t, err, "beto's did not")
	})

	t.Run("replies that block are not dropped: the user is told they are busy", func(t *testing.T) {
		s := memory.NewLiveStore(memory.Options{MaxRepliesPerUser: 2})
		for i := range 2 {
			require.True(t, s.Adopt(ana, conversationID(i)))
			require.NoError(t, s.Begin(ana, reply(conversationID(i), domain.StatusStreaming)))
		}
		require.True(t, s.Adopt(ana, conversationID(9)))

		assert.ErrorIs(t, s.Begin(ana, reply(conversationID(9), domain.StatusPending)), domain.ErrBusy)
		require.True(t, s.Adopt(beto, conv))
		assert.NoError(t, s.Begin(beto, reply(conv, domain.StatusPending)), "and beto is not affected")
	})

	t.Run("a reply nobody started is dropped to make room", func(t *testing.T) {
		c := newClock()
		s := memory.NewLiveStore(memory.Options{MaxRepliesPerUser: 1, Now: c.now})
		require.True(t, s.Adopt(ana, conversationID(1)))
		lost := reply(conversationID(1), domain.StatusPending)
		require.NoError(t, s.Begin(ana, lost))
		require.True(t, s.Adopt(ana, conversationID(2)))
		assert.ErrorIs(t, s.Begin(ana, reply(conversationID(2), domain.StatusPending)), domain.ErrBusy, "it was just made: it may still be opened")

		c.advance(time.Minute)

		assert.NoError(t, s.Begin(ana, reply(conversationID(2), domain.StatusPending)))
		_, err := s.Get(ana, conversationID(1), lost.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestForgettingAConversationForgetsItsReplies(t *testing.T) {
	c := newClock()
	s := memory.NewLiveStore(memory.Options{MaxConversationsPerUser: 1, Now: c.now})
	require.True(t, s.Adopt(ana, conv))
	r := reply(conv, domain.StatusComplete)
	require.NoError(t, s.Begin(ana, r))
	c.advance(time.Second)

	require.True(t, s.Adopt(ana, conversationID(1))) // over the limit: the first conversation goes, and its reply with it

	_, err := s.Get(ana, conv, r.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

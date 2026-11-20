// Package memory keeps what is live in this server in its memory: the replies being written, with their cards, and who started
// each conversation. It implements resource.LiveStore.
package memory

import (
	"slices"
	"sync"
	"time"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
)

var _ resource.LiveStore = (*LiveStore)(nil)

// Defaults of a LiveStore.
const (
	// DefaultTTL is how long a reply, and a conversation, is kept after it was last touched. A card that is answered later than that
	// is gone: the page says the reply is not available.
	DefaultTTL = 2 * time.Hour
	// DefaultMaxReplies and DefaultMaxConversations bound the memory.
	DefaultMaxReplies       = 10_000
	DefaultMaxConversations = 50_000
	// blockFor is how long a reply that is not finished keeps its conversation from taking another: a reply that nobody opened a
	// stream for does not block the conversation for good.
	blockFor = 15 * time.Minute
)

// Options are the limits of a LiveStore; a zero field is the default.
type Options struct {
	TTL              time.Duration
	MaxReplies       int
	MaxConversations int
	// Now is the clock.
	Now func() time.Time
}

type reply struct {
	userID  uuid.UUID
	msg     domain.Message
	touched time.Time
}

type conversation struct {
	userID  uuid.UUID
	touched time.Time
	replies []uuid.UUID
}

// LiveStore is the memory. It is safe for concurrent use.
type LiveStore struct {
	opts Options

	mu            sync.Mutex
	replies       map[uuid.UUID]*reply
	conversations map[string]*conversation
	swept         time.Time
}

// NewLiveStore returns an empty store.
func NewLiveStore(opts Options) *LiveStore {
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.MaxReplies <= 0 {
		opts.MaxReplies = DefaultMaxReplies
	}
	if opts.MaxConversations <= 0 {
		opts.MaxConversations = DefaultMaxConversations
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &LiveStore{opts: opts, replies: map[uuid.UUID]*reply{}, conversations: map[string]*conversation{}}
}

// Adopt implements resource.LiveStore.
func (s *LiveStore) Adopt(userID uuid.UUID, convID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	s.sweep(now)
	if c, ok := s.conversations[convID]; ok && s.live(c.touched, now) {
		if c.userID != userID {
			return false
		}
		c.touched = now
		return true
	}
	if _, expired := s.conversations[convID]; !expired && len(s.conversations) >= s.opts.MaxConversations {
		s.evictOldestConversation()
	}
	s.conversations[convID] = &conversation{userID: userID, touched: now}
	return true
}

// Owner implements resource.LiveStore.
func (s *LiveStore) Owner(convID string) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	s.sweep(now)
	if c, ok := s.conversations[convID]; ok && s.live(c.touched, now) {
		return c.userID, true
	}
	return uuid.Nil(), false
}

// Begin implements resource.LiveStore.
func (s *LiveStore) Begin(userID uuid.UUID, msg domain.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	s.sweep(now)
	c, ok := s.conversations[msg.ConversationID]
	if !ok || !s.live(c.touched, now) || c.userID != userID {
		return domain.ErrNotFound
	}
	c.replies = slices.DeleteFunc(c.replies, func(id uuid.UUID) bool { r, ok := s.replies[id]; return !ok || !s.live(r.touched, now) })
	for _, id := range c.replies {
		if r := s.replies[id]; r != nil && !r.msg.Status.Terminal() && now.Sub(r.touched) < blockFor {
			return domain.ErrInvalidState
		}
	}
	if len(s.replies) >= s.opts.MaxReplies && !s.evictOldestFinished() {
		return domain.ErrBusy
	}
	s.replies[msg.ID] = &reply{userID: userID, msg: msg, touched: now}
	c.replies = append(c.replies, msg.ID)
	c.touched = now
	return nil
}

// Get implements resource.LiveStore.
func (s *LiveStore) Get(userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	s.sweep(now)
	r, ok := s.find(userID, convID, msgID, now)
	if !ok {
		return domain.Message{}, domain.ErrNotFound
	}
	return r.msg, nil
}

// Update implements resource.LiveStore.
func (s *LiveStore) Update(userID uuid.UUID, convID string, msgID uuid.UUID, change func(domain.Message) (domain.Message, error)) (domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	s.sweep(now)
	r, ok := s.find(userID, convID, msgID, now)
	if !ok {
		return domain.Message{}, domain.ErrNotFound
	}
	next, err := change(r.msg)
	if err != nil {
		return r.msg, err
	}
	next.ID, next.ConversationID = r.msg.ID, r.msg.ConversationID // what it is called and where it is does not change
	r.msg, r.touched = next, now
	return next, nil
}

func (s *LiveStore) find(userID uuid.UUID, convID string, msgID uuid.UUID, now time.Time) (*reply, bool) {
	r, ok := s.replies[msgID]
	if !ok || !s.live(r.touched, now) || r.userID != userID || r.msg.ConversationID != convID {
		return nil, false
	}
	return r, true
}

// sweepEvery is how often what has expired is forgotten for good. A lookup does not wait for that: it takes what has expired for
// gone (see live).
const sweepEvery = time.Minute

// sweep forgets what has not been touched for the TTL, at most once in sweepEvery. Called with the lock held.
func (s *LiveStore) sweep(now time.Time) {
	if now.Sub(s.swept) < sweepEvery {
		return
	}
	s.swept = now
	for id, r := range s.replies {
		if now.Sub(r.touched) >= s.opts.TTL {
			delete(s.replies, id)
		}
	}
	for id, c := range s.conversations {
		if now.Sub(c.touched) >= s.opts.TTL {
			delete(s.conversations, id)
		}
	}
}

// live reports whether something touched at t is still kept at now.
func (s *LiveStore) live(t, now time.Time) bool { return now.Sub(t) < s.opts.TTL }

// evictOldestFinished drops the finished reply that was touched longest ago, and reports whether there was one.
func (s *LiveStore) evictOldestFinished() bool {
	var oldest uuid.UUID
	var at time.Time
	found := false
	for id, r := range s.replies {
		if r.msg.Status.Terminal() && (!found || r.touched.Before(at)) {
			oldest, at, found = id, r.touched, true
		}
	}
	if found {
		delete(s.replies, oldest)
	}
	return found
}

func (s *LiveStore) evictOldestConversation() {
	var oldest string
	var at time.Time
	found := false
	for id, c := range s.conversations {
		if !found || c.touched.Before(at) {
			oldest, at, found = id, c.touched, true
		}
	}
	if found {
		delete(s.conversations, oldest)
	}
}

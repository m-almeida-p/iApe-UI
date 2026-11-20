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
	// DefaultMaxReplies and DefaultMaxConversations bound the memory; the per-user limits keep one user from using it up for the rest.
	DefaultMaxReplies              = 10_000
	DefaultMaxConversations        = 50_000
	DefaultMaxRepliesPerUser       = 100
	DefaultMaxConversationsPerUser = 200
	// blockFor is how long a reply that is being written keeps its conversation from taking another turn.
	blockFor = 15 * time.Minute
	// startGrace is how long a reply that nobody has started waits for its stream, which a page opens at once: after it, the reply
	// blocks nothing and may be dropped, so that a response that was lost does not leave the conversation shut for blockFor.
	startGrace = 30 * time.Second
)

// Options are the limits of a LiveStore; a zero field is the default.
type Options struct {
	TTL              time.Duration
	MaxReplies       int
	MaxConversations int
	// MaxRepliesPerUser and MaxConversationsPerUser: a user over them loses their own oldest, not another user's.
	MaxRepliesPerUser       int
	MaxConversationsPerUser int
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
	nReplies      map[uuid.UUID]int // replies of each user
	nConvs        map[uuid.UUID]int // conversations of each user
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
	if opts.MaxRepliesPerUser <= 0 {
		opts.MaxRepliesPerUser = DefaultMaxRepliesPerUser
	}
	if opts.MaxConversationsPerUser <= 0 {
		opts.MaxConversationsPerUser = DefaultMaxConversationsPerUser
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &LiveStore{opts: opts, replies: map[uuid.UUID]*reply{}, conversations: map[string]*conversation{},
		nReplies: map[uuid.UUID]int{}, nConvs: map[uuid.UUID]int{}}
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
	s.dropConversation(convID) // one that expired
	if s.nConvs[userID] >= s.opts.MaxConversationsPerUser {
		s.evictConversation(func(c *conversation) bool { return c.userID == userID })
	}
	if len(s.conversations) >= s.opts.MaxConversations {
		s.evictConversation(func(*conversation) bool { return true })
	}
	s.conversations[convID] = &conversation{userID: userID, touched: now}
	s.nConvs[userID]++
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
		if r := s.replies[id]; r != nil && s.blocks(r, now) {
			return domain.ErrInvalidState
		}
	}
	if s.nReplies[userID] >= s.opts.MaxRepliesPerUser && !s.evictReply(now, func(r *reply) bool { return r.userID == userID }) {
		return domain.ErrBusy
	}
	if len(s.replies) >= s.opts.MaxReplies && !s.evictReply(now, func(*reply) bool { return true }) {
		return domain.ErrBusy
	}
	s.replies[msg.ID] = &reply{userID: userID, msg: msg, touched: now}
	s.nReplies[userID]++
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
	if c, ok := s.conversations[r.msg.ConversationID]; ok {
		c.touched = now // a conversation that is being written is not forgotten with the replies it holds
	}
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
			s.dropReply(id)
		}
	}
	for id, c := range s.conversations {
		if now.Sub(c.touched) >= s.opts.TTL {
			s.dropConversation(id)
		}
	}
}

// live reports whether something touched at t is still kept at now.
func (s *LiveStore) live(t, now time.Time) bool { return now.Sub(t) < s.opts.TTL }

// blocks reports whether a reply keeps its conversation from taking another turn: it is not finished, and it is being written, or it
// was started not long ago.
func (s *LiveStore) blocks(r *reply, now time.Time) bool {
	age := now.Sub(r.touched)
	if r.msg.Status.Terminal() || age >= blockFor {
		return false
	}
	return r.msg.Status != domain.StatusPending || age < startGrace
}

// evictReply drops the oldest reply that matches and that blocks nothing (see blocks), and reports whether there was one.
func (s *LiveStore) evictReply(now time.Time, match func(*reply) bool) bool {
	var oldest uuid.UUID
	var at time.Time
	found := false
	for id, r := range s.replies {
		if match(r) && !s.blocks(r, now) && (!found || r.touched.Before(at)) {
			oldest, at, found = id, r.touched, true
		}
	}
	if found {
		s.dropReply(oldest)
	}
	return found
}

// evictConversation drops the oldest conversation that matches, with its replies.
func (s *LiveStore) evictConversation(match func(*conversation) bool) {
	var oldest string
	var at time.Time
	found := false
	for id, c := range s.conversations {
		if match(c) && (!found || c.touched.Before(at)) {
			oldest, at, found = id, c.touched, true
		}
	}
	if found {
		s.dropConversation(oldest)
	}
}

func (s *LiveStore) dropReply(id uuid.UUID) {
	if r, ok := s.replies[id]; ok {
		delete(s.replies, id)
		if s.nReplies[r.userID]--; s.nReplies[r.userID] <= 0 {
			delete(s.nReplies, r.userID)
		}
	}
}

func (s *LiveStore) dropConversation(id string) {
	if c, ok := s.conversations[id]; ok {
		for _, rid := range c.replies {
			s.dropReply(rid)
		}
		delete(s.conversations, id)
		if s.nConvs[c.userID]--; s.nConvs[c.userID] <= 0 {
			delete(s.nConvs, c.userID)
		}
	}
}

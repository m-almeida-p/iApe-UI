package interaction_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/interaction"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/resource/mocks"
)

// claimRig is the service with a store and a gateway that write down what they were asked, in order: what matters about an answer
// is that the card is claimed BEFORE the service is asked, and what becomes of the claim after.
type claimRig struct {
	msgs    *mocks.MessageStore
	actions *mocks.ActionGateway
	logs    *bytes.Buffer
	svc     *interaction.Service
	now     time.Time

	mu  sync.Mutex
	log []string
}

var started = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func newClaimRig(t *testing.T) *claimRig {
	t.Helper()
	r := &claimRig{msgs: mocks.NewMessageStore(t), actions: mocks.NewActionGateway(t), logs: &bytes.Buffer{}, now: started}
	ids := 0
	r.svc = interaction.NewService(interaction.Dependencies{
		Messages: r.msgs, Actions: r.actions, Logger: slog.New(slog.NewTextHandler(r.logs, nil)),
		Now:        func() time.Time { return r.now },
		NewClaimID: func() string { ids++; return fmt.Sprintf("claim-%d", ids) },
	})
	return r
}

func (r *claimRig) saw(what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, what)
}

func (r *claimRig) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

// reads makes the store give the message once.
func (r *claimRig) reads(m domain.Message) {
	r.msgs.On("Get", mock.Anything, userID, convID, replyID).Run(func(mock.Arguments) { r.saw("get") }).Return(m, nil).Once()
}

// saves expects one save of the card from `from` to `to`, with a context that is still usable, and names it.
func (r *claimRig) saves(name string, from, to domain.Interaction, err error) {
	r.msgs.On("SaveInteraction",
		mock.MatchedBy(func(ctx context.Context) bool { return ctx.Err() == nil }),
		userID, convID, replyID, from, to).Run(func(mock.Arguments) { r.saw(name) }).Return(err).Once()
}

// claimOf is the card as it is claimed at the start of the rig by the first claim.
func claimOf(t *testing.T, card domain.Interaction, id string, at time.Time) domain.Interaction {
	t.Helper()
	claimed, err := card.Claim(id, at)
	require.NoError(t, err)
	return claimed
}

func TestClaim_TheCardIsClaimedBeforeTheServiceIsAskedAndSavedWhenItAnswers(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	claimed := claimOf(t, card, "claim-1", started)
	r.saves("claim", card, claimed, nil)
	r.actions.On("Approve", mock.Anything, card).Run(func(mock.Arguments) { r.saw("approve") }).
		Return(domain.ActionResult{Message: "Transfer paid successfully. TxnID: OP-1"}, nil).Once()
	paid := card
	paid.State, paid.Result = domain.InteractionPaid, "Transfer paid successfully. TxnID: OP-1"
	r.saves("finish", claimed, paid, nil)

	got, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.NoError(t, err)
	assert.Equal(t, []string{"get", "claim", "approve", "finish"}, r.order(), "the claim comes first: nobody else can ask the service now")
	assert.Equal(t, paid, *got.Meta.Interaction)
}

func TestClaim_AnAnswerThatLosesTheClaimNeverReachesTheService(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	r.saves("claim", card, claimOf(t, card, "claim-1", started), domain.ErrInvalidState) // another got there first

	got, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.ErrorIs(t, err, domain.ErrInvalidState)
	assert.Zero(t, got.ID, "and there is no reply to show")
	assert.Equal(t, []string{"get", "claim"}, r.order(), "the gateway was never called: that is what a double tap needs")
}

func TestClaim_ACardBeingAnsweredIsNotAnsweredAgain(t *testing.T) {
	r := newClaimRig(t)
	busy := claimOf(t, *cardIn(domain.InteractionPending), "claim-0", started.Add(-30*time.Second))
	r.reads(replyWith(&busy))

	for name, call := range map[string]func() error{
		"approve": func() error { _, err := r.svc.Approve(t.Context(), userID, convID, replyID); return err },
	} {
		err := call()

		require.ErrorIs(t, err, domain.ErrInvalidState, name)
	}
	assert.Equal(t, []string{"get"}, r.order(), "no claim, no call: it is the other answer's to finish")
}

func TestClaim_ADatabaseThatFailsToClaimStopsTheAnswerBeforeTheService(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	boom := errors.New("connection reset")
	r.saves("claim", card, claimOf(t, card, "claim-1", started), boom)

	_, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.ErrorIs(t, err, boom)
	assert.Equal(t, []string{"get", "claim"}, r.order())
}

func TestClaim_WhatTheServiceRefusesGivesTheCardBackToBeAnsweredAgain(t *testing.T) {
	refusal := &domain.ActionError{Message: "OTP verification failed"}
	for name, failure := range map[string]error{
		"it said no": refusal,
		"the request never left (no token, a refused connection)": fmt.Errorf("%w: %w: %w", domain.ErrAgentUnavailable, domain.ErrNotSent, errors.New("no VPN")),
	} {
		t.Run(name, func(t *testing.T) {
			r := newClaimRig(t)
			card := *cardIn(domain.InteractionOTPRequired)
			r.reads(replyWith(&card))
			claimed := claimOf(t, card, "claim-1", started)
			r.saves("claim", card, claimed, nil)
			r.actions.On("ConfirmOTP", mock.Anything, card, "000000").Run(func(mock.Arguments) { r.saw("service") }).
				Return(domain.ActionResult{}, failure).Once()
			r.saves("release", claimed, card, nil)

			got, err := r.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "000000")

			require.ErrorIs(t, err, failure)
			assert.Equal(t, []string{"get", "claim", "service", "release"}, r.order())
			assert.Equal(t, card, *got.Meta.Interaction, "the card is as it was, and shows the reason")
		})
	}
}

func TestClaim_WhatMayHaveReachedTheServiceKeepsTheCardClaimed(t *testing.T) {
	for name, failure := range map[string]error{
		"the service did not answer in time": fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, context.DeadlineExceeded),
		"the connection dropped":             fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, errors.New("connection reset by peer")),
		"a gateway error":                    fmt.Errorf("%w: HTTP 502", domain.ErrAgentUnavailable),
		"a plain error":                      errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newClaimRig(t)
			card := *cardIn(domain.InteractionPending)
			r.reads(replyWith(&card))
			claimed := claimOf(t, card, "claim-1", started)
			r.saves("claim", card, claimed, nil)
			r.actions.On("Approve", mock.Anything, card).Run(func(mock.Arguments) { r.saw("service") }).Return(domain.ActionResult{}, failure).Once()

			got, err := r.svc.Approve(t.Context(), userID, convID, replyID)

			require.ErrorIs(t, err, failure)
			assert.Equal(t, []string{"get", "claim", "service"}, r.order(), "the payment may have gone through: the card is not given back")
			assert.Equal(t, claimed, *got.Meta.Interaction, "the page shows it as being answered")
		})
	}
}

func TestClaim_ACardClaimedLongAgoCanBeTakenOverAndAnsweredAgain(t *testing.T) {
	r := newClaimRig(t)
	base := *cardIn(domain.InteractionPending)
	stale := claimOf(t, base, "claim-0", started.Add(-domain.ClaimTTL))
	r.reads(replyWith(&stale))
	taken, err := stale.Reclaim("claim-1", started)
	require.NoError(t, err)
	r.saves("reclaim", stale, taken, nil)
	r.actions.On("Approve", mock.Anything, base).Run(func(mock.Arguments) { r.saw("service") }).
		Return(domain.ActionResult{Message: "paid"}, nil).Once()
	paid := base
	paid.State, paid.Result = domain.InteractionPaid, "paid"
	r.saves("finish", taken, paid, nil)

	_, err = r.svc.Approve(t.Context(), userID, convID, replyID)

	require.NoError(t, err)
	assert.Equal(t, []string{"get", "reclaim", "service", "finish"}, r.order())
}

func TestClaim_AStaleClaimIsTakenOverForTheStateTheCardWentBackTo(t *testing.T) {
	// A card that was waiting for a code is answered with a code, not with a payment.
	r := newClaimRig(t)
	base := *cardIn(domain.InteractionOTPRequired)
	stale := claimOf(t, base, "claim-0", started.Add(-time.Hour))
	r.reads(replyWith(&stale))

	_, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.ErrorIs(t, err, domain.ErrInvalidState)
	assert.Equal(t, []string{"get"}, r.order())
}

func TestClaim_RejectingACardThatWaitsForACodeIsClaimedToo(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionOTPRequired)
	r.reads(replyWith(&card))
	claimed := claimOf(t, card, "claim-1", started)
	r.saves("claim", card, claimed, nil)
	r.actions.On("Reject", mock.Anything, card).Run(func(mock.Arguments) { r.saw("service") }).
		Return(domain.ActionResult{Message: "rejected"}, nil).Once()
	closed := card
	closed.State, closed.Result = domain.InteractionRejected, "rejected"
	r.saves("finish", claimed, closed, nil)

	_, err := r.svc.Reject(t.Context(), userID, convID, replyID)

	require.NoError(t, err)
	assert.Equal(t, []string{"get", "claim", "service", "finish"}, r.order())
}

func TestClaim_ACodeThatCannotBeOneClaimsNothing(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionOTPRequired)
	r.reads(replyWith(&card))

	got, err := r.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "12 34")

	var refused *domain.ActionError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, []string{"get"}, r.order(), "a mistake of the user is not worth a claim")
	assert.Equal(t, card, *got.Meta.Interaction)
}

func TestClaim_AContactChoiceThatIsNotACandidateClaimsNothing(t *testing.T) {
	r := newClaimRig(t)
	card := domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionPending, RequestID: "req-1", Query: "Maria"}
	r.reads(replyWith(&card))

	_, err := r.svc.ChooseContact(t.Context(), userID, convID, replyID, 99)

	var refused *domain.ActionError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, []string{"get"}, r.order())
}

func TestClaim_TheFinishAndTheReleaseAreSavedEvenWhenTheRequestIsGone(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	claimed := claimOf(t, card, "claim-1", started)
	r.saves("claim", card, claimed, nil)
	ctx, cancel := context.WithCancel(t.Context())
	r.actions.On("Approve", mock.Anything, card).Run(func(mock.Arguments) { cancel() }).
		Return(domain.ActionResult{}, &domain.ActionError{Message: "no"}).Once()
	r.saves("release", claimed, card, nil) // the matcher checks that this context is alive

	_, err := r.svc.Approve(ctx, userID, convID, replyID)

	var refused *domain.ActionError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, []string{"get", "claim", "release"}, r.order())
}

func TestClaim_AFinishThatIsNotSavedIsLoggedWithTheApprovalAndTheClaimIsLeftForTheTimeToFree(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	claimed := claimOf(t, card, "claim-1", started)
	r.saves("claim", card, claimed, nil)
	r.actions.On("Approve", mock.Anything, card).Return(domain.ActionResult{Message: "paid"}, nil).Once()
	paid := card
	paid.State, paid.Result = domain.InteractionPaid, "paid"
	boom := errors.New("connection reset")
	r.saves("finish", claimed, paid, boom)

	_, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.ErrorIs(t, err, boom)
	assert.Contains(t, r.logs.String(), "appr_1", "the service paid and the card does not say so: someone must know which")
}

func TestClaim_ARelease_ThatIsNotSavedIsLoggedAndTheRefusalIsStillTheAnswer(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	claimed := claimOf(t, card, "claim-1", started)
	r.saves("claim", card, claimed, nil)
	refusal := &domain.ActionError{Message: "Transfer is no longer open"}
	r.actions.On("Approve", mock.Anything, card).Return(domain.ActionResult{}, refusal).Once()
	r.saves("release", claimed, card, errors.New("connection reset"))

	_, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.ErrorIs(t, err, refusal, "the user is told what the service said")
	assert.Contains(t, r.logs.String(), "appr_1")
}

// memory is a store with the compare-and-set of the real one, behind a lock: state and claim together.
type memory struct {
	resource.MessageStore // the methods the service does not use

	mu    sync.Mutex
	msg   domain.Message
	saves int
}

func (s *memory) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.msg
	card := *m.Meta.Interaction
	m.Meta.Interaction = &card
	return m, nil
}

func (s *memory) SaveInteraction(_ context.Context, _, _, _ uuid.UUID, from, card domain.Interaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.msg.Meta.Interaction; cur.State != from.State || cur.ClaimID != from.ClaimID {
		return domain.ErrInvalidState
	}
	s.msg.Meta.Interaction = &card
	s.saves++
	return nil
}

func TestClaim_ManyAtOnceAskTheServiceOnce(t *testing.T) {
	const callers = 50
	card := *cardIn(domain.InteractionPending)
	store := &memory{msg: replyWith(&card)}
	gw := &countingGateway{}
	var ids sync.Mutex
	n := 0
	svc := interaction.NewService(interaction.Dependencies{
		Messages: store, Actions: gw, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return started },
		NewClaimID: func() string { ids.Lock(); defer ids.Unlock(); n++; return fmt.Sprintf("claim-%d", n) },
	})

	var wg sync.WaitGroup
	var won, lost, other int
	var tally sync.Mutex
	for range callers {
		wg.Go(func() {
			_, err := svc.Approve(t.Context(), userID, convID, replyID)
			tally.Lock()
			defer tally.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, domain.ErrInvalidState):
				lost++
			default:
				other++
			}
		})
	}
	wg.Wait()

	assert.Equal(t, 1, gw.calls(), "a double tap, two tabs or a phone's retries pay once: the service is asked by the one that claimed")
	assert.Equal(t, 1, won)
	assert.Equal(t, callers-1, lost)
	assert.Zero(t, other)
	assert.Equal(t, domain.InteractionPaid, store.msg.Meta.Interaction.State)
	assert.Equal(t, 2, store.saves, "one claim and one finish")
}

// countingGateway counts the answers that reach the service.
type countingGateway struct {
	resource.ActionGateway // the methods the test does not use

	mu sync.Mutex
	n  int
}

func (g *countingGateway) calls() int { g.mu.Lock(); defer g.mu.Unlock(); return g.n }

func (g *countingGateway) Approve(context.Context, domain.Interaction) (domain.ActionResult, error) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	time.Sleep(20 * time.Millisecond) // the others arrive while this one is out
	return domain.ActionResult{Message: "paid"}, nil
}

func (g *countingGateway) Reject(context.Context, domain.Interaction) (domain.ActionResult, error) {
	return domain.ActionResult{}, errors.New("not used")
}

func (g *countingGateway) ConfirmOTP(context.Context, domain.Interaction, string) (domain.ActionResult, error) {
	return domain.ActionResult{}, errors.New("not used")
}

func (g *countingGateway) ConfirmContact(context.Context, string, domain.Contact) error {
	return errors.New("not used")
}

// An answer that is sent again, or whose reply was lost, is told what the approval already is. The card takes that for its
// answer, so it shows what became of it instead of asking again.
func TestClaim_AnApprovalThatIsResolvedAlreadyIsWhatTheCardBecomes(t *testing.T) {
	approvedAlready := "This transfer was already approved. Its result is in your history."
	rejectedAlready := "This transfer was already rejected."
	for name, tc := range map[string]struct {
		call     func(*claimRig) (domain.Message, error)
		method   string
		from     domain.InteractionState
		resolved string
		state    domain.InteractionState
		result   string
	}{
		"approve, it was approved": {func(r *claimRig) (domain.Message, error) { return r.svc.Approve(t.Context(), userID, convID, replyID) },
			"Approve", domain.InteractionPending, "APPROVED", domain.InteractionPaid, approvedAlready},
		"approve, it was rejected": {func(r *claimRig) (domain.Message, error) { return r.svc.Approve(t.Context(), userID, convID, replyID) },
			"Approve", domain.InteractionPending, "REJECTED", domain.InteractionRejected, rejectedAlready},
		"reject, it was rejected": {func(r *claimRig) (domain.Message, error) { return r.svc.Reject(t.Context(), userID, convID, replyID) },
			"Reject", domain.InteractionPending, "REJECTED", domain.InteractionRejected, rejectedAlready},
		"reject, it was approved": {func(r *claimRig) (domain.Message, error) { return r.svc.Reject(t.Context(), userID, convID, replyID) },
			"Reject", domain.InteractionPending, "APPROVED", domain.InteractionPaid, approvedAlready},
		"the case of the word does not matter": {func(r *claimRig) (domain.Message, error) { return r.svc.Approve(t.Context(), userID, convID, replyID) },
			"Approve", domain.InteractionPending, "approved", domain.InteractionPaid, approvedAlready},
	} {
		t.Run(name, func(t *testing.T) {
			r := newClaimRig(t)
			card := *cardIn(tc.from)
			r.reads(replyWith(&card))
			claimed := claimOf(t, card, "claim-1", started)
			r.saves("claim", card, claimed, nil)
			r.actions.On(tc.method, mock.Anything, card).Run(func(mock.Arguments) { r.saw("service") }).
				Return(domain.ActionResult{}, &domain.ActionError{Message: "approval already resolved: " + tc.resolved, Resolved: tc.resolved}).Once()
			want := card
			want.State, want.Result = tc.state, tc.result
			r.saves("finish", claimed, want, nil)

			got, err := tc.call(r)

			require.NoError(t, err)
			assert.Equal(t, []string{"get", "claim", "service", "finish"}, r.order(), "the claim is not given back: the card says what became of it")
			assert.Equal(t, want, *got.Meta.Interaction)
		})
	}
}

func TestClaim_AResolvedApprovalThatTheCardCannotBecomeIsGivenBackWithTheReason(t *testing.T) {
	for name, tc := range map[string]struct {
		from     domain.InteractionState
		resolved string
		call     func(*claimRig) (domain.Message, error)
		method   string
	}{
		"it expired":                          {domain.InteractionPending, "EXPIRED", func(r *claimRig) (domain.Message, error) { return r.svc.Approve(t.Context(), userID, convID, replyID) }, "Approve"},
		"a word nobody knows":                 {domain.InteractionPending, "WHATEVER", func(r *claimRig) (domain.Message, error) { return r.svc.Approve(t.Context(), userID, convID, replyID) }, "Approve"},
		"approved while a code is waited for": {domain.InteractionOTPRequired, "APPROVED", func(r *claimRig) (domain.Message, error) { return r.svc.Reject(t.Context(), userID, convID, replyID) }, "Reject"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newClaimRig(t)
			card := *cardIn(tc.from)
			r.reads(replyWith(&card))
			claimed := claimOf(t, card, "claim-1", started)
			r.saves("claim", card, claimed, nil)
			refusal := &domain.ActionError{Message: "approval already resolved: " + tc.resolved, Resolved: tc.resolved}
			r.actions.On(tc.method, mock.Anything, card).Return(domain.ActionResult{}, refusal).Once()
			r.saves("release", claimed, card, nil)

			got, err := tc.call(r)

			require.ErrorIs(t, err, refusal)
			assert.Equal(t, card, *got.Meta.Interaction, "the card is as it was, with the reason")
		})
	}
}

// The save that writes what the service did is not tied to the request, so it has a time limit of its own: a store that hangs must not
// hang the answer for ever.
func TestClaim_TheFinishIsSavedWithinATimeLimit(t *testing.T) {
	r := newClaimRig(t)
	card := *cardIn(domain.InteractionPending)
	r.reads(replyWith(&card))
	claimed := claimOf(t, card, "claim-1", started)
	r.saves("claim", card, claimed, nil)
	r.actions.On("Approve", mock.Anything, card).Return(domain.ActionResult{Message: "paid"}, nil).Once()
	paid := card
	paid.State, paid.Result = domain.InteractionPaid, "paid"
	var left time.Duration
	var hasLimit bool
	r.msgs.On("SaveInteraction", mock.MatchedBy(func(ctx context.Context) bool {
		deadline, ok := ctx.Deadline()
		left, hasLimit = time.Until(deadline), ok
		return true
	}), userID, convID, replyID, claimed, paid).Return(nil).Once()

	_, err := r.svc.Approve(t.Context(), userID, convID, replyID)

	require.NoError(t, err)
	assert.True(t, hasLimit, "the context of the save has a deadline")
	assert.Positive(t, left)
	assert.LessOrEqual(t, left, 5*time.Second)
}

// Every claim has an id of its own. With the service as the app makes it (no id injected) two answers to two cards, or to one card in
// turn, are told apart by it: it is what lets the store refuse the finish of a holder whose claim was taken over.
func TestClaim_EveryClaimHasAnIDOfItsOwn(t *testing.T) {
	msgs, actions := mocks.NewMessageStore(t), mocks.NewActionGateway(t)
	svc := interaction.NewService(interaction.Dependencies{Messages: msgs, Actions: actions, Logger: slog.New(slog.DiscardHandler)})
	var ids []string
	msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyWith(cardIn(domain.InteractionPending)), nil).Twice()
	msgs.On("SaveInteraction", mock.Anything, userID, convID, replyID, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		if card := args.Get(5).(domain.Interaction); card.State == domain.InteractionProcessing {
			ids = append(ids, card.ClaimID)
		}
	}).Return(nil)
	actions.On("Approve", mock.Anything, mock.Anything).Return(domain.ActionResult{Message: "paid"}, nil)

	for range 2 {
		_, err := svc.Approve(t.Context(), userID, convID, replyID)
		require.NoError(t, err)
	}

	require.Len(t, ids, 2)
	assert.NotEmpty(t, ids[0])
	assert.NotEqual(t, ids[0], ids[1])
}

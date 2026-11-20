package aguibff_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
)

// accountTokens is a TokenSource with a token for each account, as the real one has: the token of an account is not the token of
// another. It refuses an account it does not know.
type accountTokens struct {
	mu     sync.Mutex
	tokens map[uuid.UUID]string
	asked  []uuid.UUID
}

func (a *accountTokens) Token(_ context.Context, account uuid.UUID) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, account)
	tok, ok := a.tokens[account]
	if !ok {
		return "", aguibff.ErrToken
	}
	return tok, nil
}

var (
	ana  = uuid.MustParse("a0a0a0a0-0000-4000-8000-00000000000a")
	beto = uuid.MustParse("b0b0b0b0-0000-4000-8000-00000000000b")
)

func twoAccounts() *accountTokens {
	return &accountTokens{tokens: map[uuid.UUID]string{ana: "token-of-ana", beto: "token-of-beto"}}
}

// Each request is made as the account of the user it is for, with that account's token, and with nothing of the other's.
func TestAccounts_ATurnIsMadeAsTheAccountOfItsUser(t *testing.T) {
	b := newBFF(t, serveFixture(t, "guard.sse"))
	c := clientFor(b, twoAccounts())

	for _, user := range []uuid.UUID{ana, beto} {
		_, err := collect(c.Run(t.Context(), domain.AgentRequest{UserID: user, ConversationID: uuid.New(), Text: "Hola"}))
		require.NoError(t, err)
	}

	require.Equal(t, 2, b.count())
	assert.Equal(t, ana.String(), b.requests[0].Header.Get("X-Yape-Account-Id"))
	assert.Equal(t, "Bearer token-of-ana", b.requests[0].Header.Get("Authorization"))
	assert.Equal(t, beto.String(), b.requests[1].Header.Get("X-Yape-Account-Id"))
	assert.Equal(t, "Bearer token-of-beto", b.requests[1].Header.Get("Authorization"))
}

// The answers to a card are made as the account too: the user who pays is the account that is charged.
func TestAccounts_AnAnswerToACardIsMadeAsTheAccountOfItsUser(t *testing.T) {
	b := newBFF(t, serveJSON(t, http.StatusCreated, "approve-ok.json"))
	c := clientFor(b, twoAccounts())

	_, errApprove := c.Approve(t.Context(), beto, card())
	_, errReject := c.Reject(t.Context(), ana, card())
	_, errOTP := c.ConfirmOTP(t.Context(), beto, card(), "123456")
	errContact := c.ConfirmContact(t.Context(), ana, "Maria", domain.Contact{Name: "Maria Torres", Phone: "999111222"})

	require.NoError(t, errApprove)
	require.NoError(t, errReject)
	require.NoError(t, errOTP)
	require.NoError(t, errContact)
	want := []struct {
		account uuid.UUID
		token   string
	}{{beto, "token-of-beto"}, {ana, "token-of-ana"}, {beto, "token-of-beto"}, {ana, "token-of-ana"}}
	require.Equal(t, len(want), b.count())
	for i, w := range want {
		assert.Equal(t, w.account.String(), b.requests[i].Header.Get("X-Yape-Account-Id"), b.requests[i].URL.Path)
		assert.Equal(t, "Bearer "+w.token, b.requests[i].Header.Get("Authorization"), b.requests[i].URL.Path)
	}
}

// Two users at once do not swap tokens or accounts: every request carries a pair that belongs together.
func TestAccounts_UsersAtTheSameTimeKeepTheirOwn(t *testing.T) {
	b := newBFF(t, serveFixture(t, "guard.sse"))
	c := clientFor(b, twoAccounts())
	var wg sync.WaitGroup
	for i := range 40 {
		user := []uuid.UUID{ana, beto}[i%2]
		wg.Go(func() {
			_, err := collect(c.Run(t.Context(), domain.AgentRequest{UserID: user, ConversationID: uuid.New(), Text: "Hola"}))
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	require.Equal(t, 40, b.count())
	owner := map[string]string{ana.String(): "Bearer token-of-ana", beto.String(): "Bearer token-of-beto"}
	for _, r := range b.requests {
		assert.Equal(t, owner[r.Header.Get("X-Yape-Account-Id")], r.Header.Get("Authorization"))
	}
}

// An account the token program does not know gets no request: nothing is sent as someone else.
func TestAccounts_AnAccountWithoutATokenIsNotSentAsAnotherOne(t *testing.T) {
	b := newBFF(t, serveFixture(t, "guard.sse"))
	stranger := uuid.New()

	_, err := collect(clientFor(b, twoAccounts()).Run(t.Context(), domain.AgentRequest{UserID: stranger, ConversationID: uuid.New(), Text: "Hola"}))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	require.ErrorIs(t, err, domain.ErrNotSent)
	assert.Zero(t, b.count())
}

// A request for no account is not made, and the token program is not asked: the zero UUID is what a missing user holds.
func TestAccounts_NoAccountIsNotSentAndNotAskedFor(t *testing.T) {
	b := newBFF(t, serveJSON(t, http.StatusCreated, "approve-ok.json"))
	tokens := twoAccounts()
	c := clientFor(b, tokens)

	_, errRun := collect(c.Run(t.Context(), domain.AgentRequest{ConversationID: uuid.New(), Text: "Hola"}))
	_, errApprove := c.Approve(t.Context(), uuid.Nil(), card())
	_, errReject := c.Reject(t.Context(), uuid.Nil(), card())
	_, errOTP := c.ConfirmOTP(t.Context(), uuid.Nil(), card(), "123456")
	errContact := c.ConfirmContact(t.Context(), uuid.Nil(), "q", domain.Contact{Name: "n", Phone: "1"})

	for _, err := range []error{errRun, errApprove, errReject, errOTP, errContact} {
		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		require.ErrorIs(t, err, domain.ErrNotSent, "nothing left: the card may be answered again")
	}
	assert.Zero(t, b.count())
	assert.Empty(t, tokens.asked)
}

// What goes wrong says what failed and never who it failed for or with which token.
func TestAccounts_ErrorsHoldNeitherTheAccountNorTheToken(t *testing.T) {
	b := newBFF(t, serveFixture(t, "guard.sse"))
	stranger := uuid.New()

	_, err := collect(clientFor(b, twoAccounts()).Run(t.Context(), domain.AgentRequest{UserID: stranger, ConversationID: uuid.New(), Text: "Hola"}))

	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), stranger.String()), err.Error())
	assert.NotContains(t, err.Error(), "token-of")
}

// A refreshed token is asked for the account that had the 401, and the other account is not touched.
func TestAccounts_ARefusedTokenIsRefreshedForItsOwnAccount(t *testing.T) {
	tokens := &fakeTokens{token: "stale", fresh: "fresh"}
	b := newBFF(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("Authorization") == "Bearer stale" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serveFixture(t, "guard.sse")(w, r, n)
	})

	_, err := collect(clientFor(b, tokens).Run(t.Context(), domain.AgentRequest{UserID: beto, ConversationID: uuid.New(), Text: "Hola"}))

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{beto, beto}, tokens.asked, "the token and its refresh were both asked for the account of the turn")
	assert.Equal(t, beto.String(), b.requests[1].Header.Get("X-Yape-Account-Id"))
}

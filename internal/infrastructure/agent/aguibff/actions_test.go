package aguibff_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/infrastructure/agent/aguibff"
)

var _ resource.ActionGateway = (*aguibff.Client)(nil)

func card() domain.Interaction {
	return domain.Interaction{
		Kind: domain.InteractionApproval, State: domain.InteractionPending,
		ApprovalID: "appr_28006196-bd87-4d46-931e-0f74a46b2c57", RunID: "run-1",
	}
}

func serveJSON(t *testing.T, status int, name string) func(http.ResponseWriter, *http.Request, int) {
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestActions_ApprovePaysTheTransferAndSaysWhatTheServiceSaid(t *testing.T) {
	b := newBFF(t, serveJSON(t, http.StatusCreated, "approve-ok.json"))

	got, err := clientFor(b, &fakeTokens{token: "tok-1"}).Approve(t.Context(), accountID, card())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionResult{Message: "Transfer paid successfully. TxnID: OP-144183D869"}, got)
	require.Equal(t, 1, b.count())
	r := b.requests[0]
	assert.Equal(t, http.MethodPost, r.Method)
	assert.Equal(t, "/bff/approvals/appr_28006196-bd87-4d46-931e-0f74a46b2c57/approve", r.URL.Path)
	assert.Equal(t, "Bearer tok-1", r.Header.Get("Authorization"))
	assert.Equal(t, account, r.Header.Get("X-Yape-Account-Id"))
	var sent map[string]string
	require.NoError(t, json.Unmarshal(b.bodies[0], &sent))
	assert.Equal(t, map[string]string{"run_id": "run-1"}, sent)
}

func TestActions_ApproveThatNeedsACodeNamesTheChallenge(t *testing.T) {
	b := newBFF(t, serveJSON(t, http.StatusCreated, "approve-otp.json"))

	got, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionResult{
		Message:     "Transfer approved. Enter the OTP to continue.",
		ChallengeID: "1ef854dc-6dba-467b-903a-6ac7c4461439",
	}, got, "a real response of the BFF: the transfer_id is the challenge")
}

func TestActions_ACodeIsAskedForWithoutSayingWhichTransferIsAnError(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"success":true,"otp_required":true,"displayMessage":"x"}`)
	})

	_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "transfer_id")
}

func TestActions_RejectCancelsTheTransfer(t *testing.T) {
	b := newBFF(t, serveJSON(t, http.StatusCreated, "reject-ok.json"))

	got, err := clientFor(b, &fakeTokens{token: "t"}).Reject(t.Context(), accountID, card())

	require.NoError(t, err)
	assert.Equal(t, "Transfer rejected successfully.", got.Message)
	assert.Equal(t, "/bff/approvals/appr_28006196-bd87-4d46-931e-0f74a46b2c57/reject", b.requests[0].URL.Path)
}

func TestActions_TheCodeGoesWithTheChallengeAndTheRun(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"success":true,"execution":{"provider_txn_id":"OP-1","status":"SUCCESS"},"displayMessage":"Transfer paid successfully. TxnID: OP-1"}`)
	})
	c := card()
	c.State, c.ChallengeID = domain.InteractionOTPRequired, "trf_9"

	got, err := clientFor(b, &fakeTokens{token: "t"}).ConfirmOTP(t.Context(), accountID, c, "123456")

	require.NoError(t, err)
	assert.Equal(t, "Transfer paid successfully. TxnID: OP-1", got.Message)
	assert.Equal(t, "/bff/approvals/appr_28006196-bd87-4d46-931e-0f74a46b2c57/otp/confirm", b.requests[0].URL.Path)
	var sent map[string]string
	require.NoError(t, json.Unmarshal(b.bodies[0], &sent))
	assert.Equal(t, map[string]string{"transfer_id": "trf_9", "otp": "123456", "run_id": "run-1"}, sent)
}

func TestActions_ARefusalIsTheServicesOwnWordsForTheUser(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"statusCode":400,"message":"OTP verification failed","error":"Bad Request"}`)
	})
	c := card()
	c.State, c.ChallengeID = domain.InteractionOTPRequired, "trf_9"

	_, err := clientFor(b, &fakeTokens{token: "t"}).ConfirmOTP(t.Context(), accountID, c, "000000")

	var refused *domain.ActionError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "OTP verification failed", refused.Message)
}

func TestActions_AGatewayFailureIsTheAgentBeingUnavailable(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusBadGateway) })

	_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
}

func TestActions_AnExpiredTokenIsRefreshedOnce(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		assert.Equal(t, "Bearer fresh", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"success":true,"displayMessage":"Transfer rejected successfully."}`)
	})
	tokens := &fakeTokens{token: "old", fresh: "fresh"}

	got, err := clientFor(b, tokens).Reject(t.Context(), accountID, card())

	require.NoError(t, err)
	assert.Equal(t, "Transfer rejected successfully.", got.Message)
	assert.Equal(t, 1, tokens.refreshed)
	assert.Equal(t, 2, b.count())
}

func TestActions_TheTokenIsNeverInAnError(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusInternalServerError) })

	_, err := clientFor(b, &fakeTokens{token: "secret-token-xyz"}).Approve(t.Context(), accountID, card())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-token-xyz")
}

func TestActions_AnApprovalIdWithPathCharactersCannotChangeTheRoute(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"success":true,"displayMessage":"ok"}`)
	})
	c := card()
	c.ApprovalID = "../../admin/x?y=1"

	_, err := clientFor(b, &fakeTokens{token: "t"}).Reject(t.Context(), accountID, c)

	require.NoError(t, err)
	assert.Equal(t, "/bff/approvals/..%2F..%2Fadmin%2Fx%3Fy=1/reject", b.requests[0].URL.EscapedPath(), "the id stays one path segment")
}

func TestActions_ConfirmContactTellsTheBFFWhomTheUserChose(t *testing.T) {
	b := newBFF(t, serveJSON(t, http.StatusCreated, "contact-ok.json"))

	err := clientFor(b, &fakeTokens{token: "tok-1"}).ConfirmContact(t.Context(), accountID, "Maria", domain.Contact{Name: "Maria Torres", Phone: "912345678"})

	require.NoError(t, err)
	require.Equal(t, 1, b.count())
	r := b.requests[0]
	assert.Equal(t, http.MethodPost, r.Method)
	assert.Equal(t, "/bff/contacts/confirm", r.URL.Path)
	assert.Equal(t, "Bearer tok-1", r.Header.Get("Authorization"))
	assert.Equal(t, account, r.Header.Get("X-Yape-Account-Id"))
	var sent map[string]string
	require.NoError(t, json.Unmarshal(b.bodies[0], &sent))
	assert.Equal(t, map[string]string{"query": "Maria", "phone": "912345678", "name": "Maria Torres"}, sent)
}

func TestActions_AContactTheBFFRefusesIsTheServicesOwnWords(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"statusCode":400,"message":["phone must be valid","name must not be empty"]}`)
	})

	err := clientFor(b, &fakeTokens{token: "t"}).ConfirmContact(t.Context(), accountID, "Maria", domain.Contact{})

	var refused *domain.ActionError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "phone must be valid; name must not be empty", refused.Message)
}

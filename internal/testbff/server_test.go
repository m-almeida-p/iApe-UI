package testbff_test

import (
	"bufio"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/testbff"
)

func run(t *testing.T, h http.Handler, text string) []domain.AgentEvent {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	body := `{"threadId":"t1","runId":"r1","state":{},"tools":[],"context":[],"forwardedProps":{},"messages":[{"id":"m1","role":"user","content":` + quote(text) + `}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Yape-Account-Id", "25906ac5-9f18-4099-b3fd-e1b74d8549a4")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "the BFF answers a stream with 201")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	var events []domain.AgentEvent
	for ev, err := range aguibff.Decode(resp.Body) {
		require.NoError(t, err)
		events = append(events, ev)
	}
	return events
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func finalText(events []domain.AgentEvent) string {
	var out []string
	for _, e := range events {
		if e.Kind == domain.AgentFinal {
			out = append(out, e.Text)
		}
	}
	return strings.Join(out, "\n\n")
}

func TestHandler_AnswersWhatItWasToldWordByWord(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))

	events := run(t, h, "hola mundo")

	assert.Equal(t, "You said: hola mundo", finalText(events))
	var deltas int
	for _, e := range events {
		if e.Kind == domain.AgentDelta {
			deltas++
		}
	}
	assert.Greater(t, deltas, 1, "the text comes in pieces, as a model's does")
	assert.EqualValues(t, 1, h.Probe().Runs())
}

func TestHandler_TheMarkersChooseWhatItDoes(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0), testbff.WithSlow(0, 5))

	t.Run("#markdown", func(t *testing.T) {
		assert.Contains(t, finalText(run(t, h, "give me #markdown")), "| Name | Value |")
	})
	t.Run("#html is the page's problem, not the agent's", func(t *testing.T) {
		assert.Contains(t, finalText(run(t, h, "#html")), "<script>alert(1)</script>")
	})
	t.Run("#slow is a long reply of ticks", func(t *testing.T) {
		assert.Contains(t, finalText(run(t, h, "#slow")), "tick 5")
	})
	t.Run("#tool calls a tool and then answers with what it returned", func(t *testing.T) {
		events := run(t, h, "#tool find")

		var tools []string
		for _, e := range events {
			if e.Kind == domain.AgentToolCall {
				tools = append(tools, e.Tool)
			}
		}
		assert.Equal(t, []string{"lookup"}, tools)
		assert.Contains(t, finalText(events), "The lookup returned")
	})
	t.Run("#error is the agent's own error, as the service reports it", func(t *testing.T) {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent",
			strings.NewReader(`{"threadId":"t","runId":"r","messages":[{"role":"user","content":"#error"}]}`))
		req.Header.Set("Authorization", "Bearer x")
		req.Header.Set("X-Yape-Account-Id", "a")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		var failure error
		for _, err := range aguibff.Decode(resp.Body) {
			if err != nil {
				failure = err
			}
		}

		var agentErr *domain.AgentError
		require.ErrorAs(t, failure, &agentErr)
		assert.Contains(t, agentErr.Detail, "#error")
	})
}

func TestHandler_TheCardsTheWebUIDraws(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))

	t.Run("#pay is a transfer to approve", func(t *testing.T) {
		events := run(t, h, "#pay")

		var cards []domain.Interaction
		for _, e := range events {
			if e.Kind == domain.AgentInteraction {
				cards = append(cards, e.Interaction)
			}
		}
		require.Len(t, cards, 1)
		assert.Equal(t, domain.InteractionApproval, cards[0].Kind)
		assert.Equal(t, "appr_pay", cards[0].ApprovalID)
		assert.Equal(t, "r1", cards[0].RunID)
		assert.Equal(t, 20.0, cards[0].Transfer.Amount)
	})
	t.Run("#otp is a transfer that asks for a code", func(t *testing.T) {
		events := run(t, h, "#otp")

		var id string
		for _, e := range events {
			if e.Kind == domain.AgentInteraction {
				id = e.Interaction.ApprovalID
			}
		}
		assert.Equal(t, "appr_otp", id)
	})
	t.Run("#contact is a contact to confirm", func(t *testing.T) {
		events := run(t, h, "#contact")

		var card domain.Interaction
		for _, e := range events {
			if e.Kind == domain.AgentInteraction {
				card = e.Interaction
			}
		}
		assert.Equal(t, domain.InteractionContact, card.Kind)
		assert.Equal(t, "Maria", card.Query)
	})
}

func post(t *testing.T, h http.Handler, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Yape-Account-Id", "25906ac5-9f18-4099-b3fd-e1b74d8549a4")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_ActionsAsTheServiceAnswersThem(t *testing.T) {
	h := testbff.NewHandler()

	t.Run("approving pays", func(t *testing.T) {
		rec := post(t, h, "/bff/approvals/appr_pay/approve", `{"run_id":"r1"}`)

		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Contains(t, rec.Body.String(), "Transfer paid successfully")
		assert.Contains(t, rec.Body.String(), `"otp_required":false`)
	})
	t.Run("approving one that needs a code names the transfer", func(t *testing.T) {
		rec := post(t, h, "/bff/approvals/appr_otp/approve", `{"run_id":"r1"}`)

		assert.Contains(t, rec.Body.String(), `"otp_required":true`)
		assert.Contains(t, rec.Body.String(), `"transfer_id":"trf_otp"`)
	})
	t.Run("rejecting", func(t *testing.T) {
		rec := post(t, h, "/bff/approvals/appr_pay/reject", `{"run_id":"r1"}`)

		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Contains(t, rec.Body.String(), "Transfer rejected successfully")
	})
	t.Run("the right code pays, a wrong one is refused in the service's words", func(t *testing.T) {
		ok := post(t, h, "/bff/approvals/appr_otp/otp/confirm", `{"transfer_id":"trf_otp","otp":"123456","run_id":"r1"}`)
		bad := post(t, h, "/bff/approvals/appr_otp/otp/confirm", `{"transfer_id":"trf_otp","otp":"000000","run_id":"r1"}`)
		missing := post(t, h, "/bff/approvals/appr_otp/otp/confirm", `{"otp":"123456"}`)

		assert.Equal(t, http.StatusCreated, ok.Code)
		assert.Contains(t, ok.Body.String(), "Transfer paid successfully")
		assert.Equal(t, http.StatusBadRequest, bad.Code)
		assert.Contains(t, bad.Body.String(), "OTP verification failed")
		assert.Equal(t, http.StatusBadRequest, missing.Code)
		assert.Contains(t, missing.Body.String(), "Missing transfer_id or otp")
	})
	t.Run("a contact needs its query, phone and name", func(t *testing.T) {
		ok := post(t, h, "/bff/contacts/confirm", `{"query":"Maria","phone":"912345678","name":"Maria Torres"}`)
		bad := post(t, h, "/bff/contacts/confirm", `{"name":"Maria Torres"}`)

		assert.Equal(t, http.StatusCreated, ok.Code)
		assert.Equal(t, http.StatusBadRequest, bad.Code)
		assert.Contains(t, bad.Body.String(), "Missing required fields: query, phone")
	})
}

func TestHandler_AskingWithoutAnAccountOrATokenIs401(t *testing.T) {
	h := testbff.NewHandler()

	for _, path := range []string{"/agent", "/bff/approvals/x/approve", "/bff/contacts/confirm"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
	}
}

func TestHandler_Health(t *testing.T) {
	rec := httptest.NewRecorder()

	testbff.NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandler_AClientThatLeavesIsCounted(t *testing.T) {
	h := testbff.NewHandler(testbff.WithSlow(20*time.Millisecond, 500))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent",
		strings.NewReader(`{"threadId":"t","runId":"r","messages":[{"role":"user","content":"#slow"}]}`))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("X-Yape-Account-Id", "a")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, err = bufio.NewReader(resp.Body).ReadString('\n') // it has begun
	require.NoError(t, err)

	_ = resp.Body.Close() // and the client goes

	require.Eventually(t, func() bool { return h.Probe().Cancelled() == 1 }, 3*time.Second, 10*time.Millisecond)
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestHandler_ACutReplyEndsWithoutSayingTheRunFinished(t *testing.T) {
	h := testbff.NewHandler(testbff.WithSlow(10*time.Millisecond, 100), testbff.WithCutAfter(60*time.Millisecond))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent",
		strings.NewReader(`{"threadId":"t","runId":"r","messages":[{"role":"user","content":"#slow"}]}`))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("X-Yape-Account-Id", "a")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var got []domain.AgentEvent
	var failure error
	for ev, err := range aguibff.Decode(resp.Body) {
		if err != nil {
			failure = err
			break
		}
		got = append(got, ev)
	}

	require.ErrorIs(t, failure, domain.ErrIncompleteStream)
	assert.NotEmpty(t, got, "some of the reply arrived")
	assert.Less(t, len(got), 100, "and not all of it")
	assert.Zero(t, h.Probe().Cancelled(), "the client did not leave: the server cut it off")
}

package aguibff_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
)

// answerWith serves a status and a body to an action.
func answerWith(status int, body string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestActions_TheRefusalsMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // empty: it is not the service's words
	}{
		{"a string", `{"message":"Insufficient balance"}`, "Insufficient balance"},
		{"a list of strings", `{"message":["a must be valid","b is required"]}`, "a must be valid; b is required"},
		{"a list with one string", `{"message":["only one"]}`, "only one"},
		{"a list with strings and other things: the strings are kept", `{"message":["a",5,null,{"x":1},"b"]}`, "a; b"},
		{"a list with no strings", `{"message":[1,2,{"x":1}]}`, ""},
		{"an empty string", `{"message":""}`, ""},
		{"an empty list", `{"message":[]}`, ""},
		{"null", `{"message":null}`, ""},
		{"an object", `{"message":{"code":"E1","text":"no"}}`, ""},
		{"a number", `{"message":42}`, ""},
		{"a boolean", `{"message":true}`, ""},
		{"no message", `{"statusCode":400,"error":"Bad Request"}`, ""},
		{"HTML", `<html><body>Bad Request</body></html>`, ""},
		{"an empty body", ``, ""},
		{"JSON that is not an object", `["message"]`, ""},
		{"invalid JSON", `{"message":"cut`, ""},
		{"unicode and markup, verbatim", `{"message":"Saldo <b>insuficiente</b> 💸 ñ"}`, "Saldo <b>insuficiente</b> 💸 ñ"},
		{"a message longer than the error excerpt", `{"message":"` + strings.Repeat("palabra ", 200) + `fin"}`, strings.Repeat("palabra ", 200) + "fin"},
		{"a long list of validation messages", `{"message":[` + strings.TrimSuffix(strings.Repeat(`"field must be a valid value of the expected kind",`, 30), ",") + `]}`, strings.TrimSuffix(strings.Repeat("field must be a valid value of the expected kind; ", 30), "; ")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusBadRequest, tc.body))

			_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

			require.Error(t, err)
			refused, isRefusal := errors.AsType[*domain.ActionError](err)
			if tc.want == "" {
				assert.False(t, isRefusal, "%v", err)
				httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
				require.True(t, ok, "it stays the HTTP error: %v", err)
				assert.Equal(t, http.StatusBadRequest, httpErr.Status)
				return
			}
			require.True(t, isRefusal, "%v", err)
			assert.Equal(t, tc.want, refused.Message)
		})
	}
}

func TestActions_EveryStatus(t *testing.T) {
	const explained = `{"message":"the service explains"}`
	tests := []struct {
		status      int
		wantRefusal bool
		wantDown    bool
	}{
		{status: http.StatusBadRequest, wantRefusal: true},
		{status: http.StatusNotFound, wantRefusal: true},
		{status: http.StatusConflict, wantRefusal: true},
		{status: http.StatusUnprocessableEntity, wantRefusal: true},
		{status: http.StatusTooManyRequests, wantRefusal: true},
		{status: http.StatusInternalServerError},
		{status: http.StatusBadGateway, wantDown: true},
		{status: http.StatusServiceUnavailable, wantDown: true},
		{status: http.StatusGatewayTimeout, wantDown: true},
		{status: http.StatusFound},
		{status: http.StatusNotModified},
		{status: http.StatusMultipleChoices},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			b := newBFF(t, answerWith(tc.status, explained))

			_, err := clientFor(b, &fakeTokens{token: "t"}).Reject(t.Context(), accountID, card())

			require.Error(t, err)
			_, isRefusal := errors.AsType[*domain.ActionError](err)
			assert.Equal(t, tc.wantRefusal, isRefusal, "%v", err)
			assert.Equal(t, tc.wantDown, errors.Is(err, domain.ErrAgentUnavailable), "%v", err)
			httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
			if !tc.wantRefusal {
				require.True(t, ok, "%v", err)
				assert.Equal(t, tc.status, httpErr.Status)
			}
		})
	}
}

func TestActions_AnErrorBodyOf10MBIsBounded(t *testing.T) {
	b := newBFF(t, answerWith(http.StatusInternalServerError, strings.Repeat("x", 10<<20)))

	_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

	httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
	require.True(t, ok)
	assert.LessOrEqual(t, len(httpErr.Body), 512)
}

func TestActions_ARefusalWithABigBodyIsStillBounded(t *testing.T) {
	b := newBFF(t, answerWith(http.StatusBadRequest, `{"message":"`+strings.Repeat("x", 10<<20)+`"}`))

	_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

	require.Error(t, err)
	if refused, ok := errors.AsType[*domain.ActionError](err); ok {
		assert.Less(t, len(refused.Message), 1<<20)
	}
}

func TestActions_AnApprovalIdIsOnePathSegmentWhateverItHolds(t *testing.T) {
	tests := []struct {
		id   string
		want string // the escaped path the BFF got
	}{
		{"appr_1", "/bff/approvals/appr_1/reject"},
		{"a b", "/bff/approvals/a%20b/reject"},
		{"100%", "/bff/approvals/100%25/reject"},
		{"a?b=1", "/bff/approvals/a%3Fb=1/reject"},
		{"a#b", "/bff/approvals/a%23b/reject"},
		{"a/b", "/bff/approvals/a%2Fb/reject"},
		{"a\\b", "/bff/approvals/a%5Cb/reject"},
		{"ñandú-🦤", "/bff/approvals/%C3%B1and%C3%BA-%F0%9F%A6%A4/reject"},
		{"a\nb", "/bff/approvals/a%0Ab/reject"},
		{"%2e%2e", "/bff/approvals/%252e%252e/reject"},
		{"a.b", "/bff/approvals/a.b/reject"},
		{"...", "/bff/approvals/.../reject"},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusCreated, `{"success":true,"displayMessage":"ok"}`))
			c := card()
			c.ApprovalID = tc.id

			_, err := clientFor(b, &fakeTokens{token: "t"}).Reject(t.Context(), accountID, c)

			require.NoError(t, err)
			assert.Equal(t, tc.want, b.requests[0].URL.EscapedPath())
		})
	}
}

func TestActions_AnApprovalIdThatWouldMoveTheRouteIsRefusedAndNothingIsSent(t *testing.T) {
	for _, id := range []string{"", ".", "..", " ", "\t"} {
		t.Run("id "+id, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusCreated, `{"success":true,"displayMessage":"ok"}`))
			c := card()
			c.ApprovalID = id
			client := clientFor(b, &fakeTokens{token: "t"})

			_, errApprove := client.Approve(t.Context(), accountID, c)
			_, errReject := client.Reject(t.Context(), accountID, c)
			_, errOTP := client.ConfirmOTP(t.Context(), accountID, c, "123456")

			for _, err := range []error{errApprove, errReject, errOTP} {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "approval")
			}
			assert.Zero(t, b.count(), "a route with a hole or a dot in it is another route")
		})
	}
}

func TestActions_TheBodyIsJSONWhateverTheUserTyped(t *testing.T) {
	for name, code := range map[string]string{
		"quotes":        `12"34`,
		"backslashes":   `\12\\34`,
		"unicode":       "１２３４５６ 🔐 ñ",
		"line breaks":   "12\n34\r\n",
		"JSON":          `","run_id":"other","x":"`,
		"empty":         "",
		"very long":     strings.Repeat("9", 1<<20),
		"control chars": "1\x002\x1f3",
	} {
		t.Run(name, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusCreated, `{"success":true,"displayMessage":"ok"}`))
			c := card()
			c.ChallengeID = "trf_9"

			_, err := clientFor(b, &fakeTokens{token: "t"}).ConfirmOTP(t.Context(), accountID, c, code)

			require.NoError(t, err)
			var sent map[string]string
			require.NoError(t, json.Unmarshal(b.bodies[0], &sent))
			assert.Equal(t, map[string]string{"transfer_id": "trf_9", "otp": code, "run_id": "run-1"}, sent, "the code cannot add fields")
		})
	}
}

func TestActions_AnEmptyRunIDIsSentAsEmpty(t *testing.T) {
	b := newBFF(t, answerWith(http.StatusCreated, `{"success":true,"displayMessage":"ok"}`))
	c := card()
	c.RunID = ""

	_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, c)

	require.NoError(t, err)
	var sent map[string]string
	require.NoError(t, json.Unmarshal(b.bodies[0], &sent))
	assert.Equal(t, map[string]string{"run_id": ""}, sent)
}

func TestActions_ConfirmContactSendsTheTextAsItIs(t *testing.T) {
	for name, c := range map[string]domain.Contact{
		"quotes":     {Name: `María "la Jefa" O'Neil`, Phone: `9"87`},
		"unicode":    {Name: "李小龍 🥋 Ñandú", Phone: "+51 987 654 321"},
		"line break": {Name: "Maria\nTorres\r\n", Phone: "987\t654"},
		"backslash":  {Name: `a\b\\c`, Phone: "1"},
		"empty":      {},
		"markup":     {Name: "<script>alert(1)</script>", Phone: "1"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusCreated, `{"status":"ok"}`))

			err := clientFor(b, &fakeTokens{token: "t"}).ConfirmContact(t.Context(), accountID, "Ma\"ria\n", c)

			require.NoError(t, err)
			var sent map[string]string
			require.NoError(t, json.Unmarshal(b.bodies[0], &sent))
			assert.Equal(t, map[string]string{"query": "Ma\"ria\n", "phone": c.Phone, "name": c.Name}, sent)
		})
	}
}

func TestActions_TextThatIsNotUTF8IsAnErrorAndNothingIsSent(t *testing.T) {
	b := newBFF(t, answerWith(http.StatusCreated, `{"status":"ok"}`))
	client := clientFor(b, &fakeTokens{token: "t"})
	c := card()

	_, errOTP := client.ConfirmOTP(t.Context(), accountID, c, "12\xff34")
	errContact := client.ConfirmContact(t.Context(), accountID, "q", domain.Contact{Name: "\xc3\x28"})

	require.Error(t, errOTP)
	require.Error(t, errContact)
	assert.Zero(t, b.count())
}

func TestActions_ASuccessThatCannotBeRead(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"an empty body", ``},
		{"a body that is not JSON", `OK`},
		{"HTML", `<html>ok</html>`},
		{"a JSON array", `[{"displayMessage":"ok"}]`},
		{"a JSON string", `"ok"`},
		{"a message that is a number", `{"displayMessage":5}`},
		{"otp_required that is a string", `{"otp_required":"true"}`},
		{"JSON followed by something else", `{"displayMessage":"ok"} {"x":1}`},
		{"cut JSON", `{"displayMessage":"o`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusCreated, tc.body))

			_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

			require.Error(t, err)
			assert.Contains(t, err.Error(), "reading the BFF's answer")
			assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
			_, isRefusal := errors.AsType[*domain.ActionError](err)
			assert.False(t, isRefusal, "a success the client cannot read is not the service refusing")
		})
	}
}

func TestActions_ASuccessWithNoMessageHasNoMessage(t *testing.T) {
	for _, body := range []string{`{}`, `{"success":true}`, `{"displayMessage":null}`, `{"displayMessage":"ok","future":{"a":[1]}}`} {
		t.Run(body, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusOK, body))

			got, err := clientFor(b, &fakeTokens{token: "t"}).Reject(t.Context(), accountID, card())

			require.NoError(t, err)
			assert.Equal(t, strings.Contains(body, `"ok"`), got.Message == "ok")
		})
	}
}

func TestActions_ASuccessBodyOfTenMegabytesIsNotRead(t *testing.T) {
	b := newBFF(t, answerWith(http.StatusCreated, `{"displayMessage":"`+strings.Repeat("x", 10<<20)+`"}`))

	_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

	require.Error(t, err)
}

func TestActions_ACodeIsAskedFor(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      domain.ActionResult
		wantError string
	}{
		{"with its transfer", `{"otp_required":true,"transfer_id":"trf_1","displayMessage":"code"}`, domain.ActionResult{Message: "code", ChallengeID: "trf_1"}, ""},
		{"with an empty transfer_id", `{"otp_required":true,"transfer_id":""}`, domain.ActionResult{}, "transfer_id"},
		{"with a null transfer_id", `{"otp_required":true,"transfer_id":null}`, domain.ActionResult{}, "transfer_id"},
		{"with a transfer_id of spaces", `{"otp_required":true,"transfer_id":"   "}`, domain.ActionResult{}, "transfer_id"},
		{"with a transfer_id of a tab and a line break", `{"otp_required":true,"transfer_id":"\t\n"}`, domain.ActionResult{}, "transfer_id"},
		{"with no transfer_id", `{"otp_required":true}`, domain.ActionResult{}, "transfer_id"},
		{"not asked, whatever transfer_id says", `{"otp_required":false,"transfer_id":"trf_1","displayMessage":"paid"}`, domain.ActionResult{Message: "paid"}, ""},
		{"not mentioned", `{"transfer_id":"trf_1","displayMessage":"paid"}`, domain.ActionResult{Message: "paid"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, answerWith(http.StatusCreated, tc.body))

			got, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

			if tc.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantError)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestActions_TheCallerLeavingIsItsOwnErrorNotARefusal(t *testing.T) {
	t.Run("before the request", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusCreated, `{}`))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(ctx, accountID, card())

		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, domain.ErrAgentUnavailable, "the user left: the service did nothing wrong")
		_, isRefusal := errors.AsType[*domain.ActionError](err)
		assert.False(t, isRefusal)
	})
	t.Run("in the middle of it", func(t *testing.T) {
		arrived := make(chan struct{})
		b := newBFF(t, func(_ http.ResponseWriter, r *http.Request, _ int) { close(arrived); <-r.Context().Done() })
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-arrived; cancel() }()

		_, err := clientFor(b, &fakeTokens{token: "t"}).Reject(ctx, accountID, card())

		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
		_, isRefusal := errors.AsType[*domain.ActionError](err)
		assert.False(t, isRefusal)
	})
	t.Run("in the middle of the answer", func(t *testing.T) {
		started := make(chan struct{})
		b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"displayMessage":"par`)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		})
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-started; cancel() }()

		_, err := clientFor(b, &fakeTokens{token: "t"}).Reject(ctx, accountID, card())

		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestActions_TheTimeout(t *testing.T) {
	t.Run("a service that does not answer is a timeout", func(t *testing.T) {
		b := newBFF(t, func(_ http.ResponseWriter, r *http.Request, _ int) { <-r.Context().Done() })

		_, err := clientAt(b.srv.URL, 100*time.Millisecond, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "timed out")
	})
	t.Run("a tiny limit is a timeout", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusCreated, `{}`))

		_, err := clientAt(b.srv.URL, time.Nanosecond, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("zero is no limit", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusCreated, `{"displayMessage":"ok"}`))

		got, err := clientAt(b.srv.URL, 0, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

		require.NoError(t, err)
		assert.Equal(t, "ok", got.Message)
	})
}

func TestActions_TheTokenSource(t *testing.T) {
	t.Run("a token source that fails means nothing is sent", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusCreated, `{}`))

		_, err := clientFor(b, plainTokens{err: aguibff.ErrToken}).Approve(t.Context(), accountID, card())

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Zero(t, b.count())
	})
	t.Run("an empty token means nothing is sent", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusCreated, `{}`))

		err := clientFor(b, plainTokens{}).ConfirmContact(t.Context(), accountID, "q", domain.Contact{})

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Zero(t, b.count())
	})
	t.Run("a 401 twice is an error, with one refresh, and not the service's words for the user", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusUnauthorized, `{"statusCode":401,"message":"Unauthorized"}`))
		tokens := &fakeTokens{token: canary, fresh: canary}

		_, err := clientFor(b, tokens).Approve(t.Context(), accountID, card())

		require.Error(t, err)
		assert.Equal(t, 1, tokens.refreshed)
		assert.Equal(t, 2, b.count())
		assert.NotContains(t, err.Error(), canary)
	})
	t.Run("a refused token is an error when the source cannot refresh, after one request", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusUnauthorized, `{}`))

		_, err := clientFor(b, plainTokens{token: "old"}).Reject(t.Context(), accountID, card())

		require.Error(t, err)
		assert.Equal(t, 1, b.count())
	})
	t.Run("a refresh that fails is an unavailable agent", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusUnauthorized, `{}`))
		tokens := &refreshFails{}

		_, err := clientFor(b, tokens).Reject(t.Context(), accountID, card())

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, 1, b.count())
	})
}

func TestActions_TheTokenIsNeverInAnyError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			b := newBFF(t, answerWith(status, `{"message":"nope"}`))
			client := clientFor(b, &fakeTokens{token: canary, fresh: canary})

			_, e1 := client.Approve(t.Context(), accountID, card())
			_, e2 := client.Reject(t.Context(), accountID, card())
			_, e3 := client.ConfirmOTP(t.Context(), accountID, card(), "1")
			e4 := client.ConfirmContact(t.Context(), accountID, "q", domain.Contact{})

			for _, err := range []error{e1, e2, e3, e4} {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), canary)
			}
		})
	}
	t.Run("an unreachable BFF", func(t *testing.T) {
		b := newBFF(t, answerWith(http.StatusOK, `{}`))
		c := clientFor(b, &fakeTokens{token: canary})
		b.srv.Close()

		_, err := c.Approve(t.Context(), accountID, card())

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.NotContains(t, err.Error(), canary)
	})
}

func TestActions_TheRequestIsMadeAsTheAccount(t *testing.T) {
	b := newBFF(t, answerWith(http.StatusCreated, `{"displayMessage":"ok"}`))
	client := clientAt(b.srv.URL+"/api/", time.Second, &fakeTokens{token: "tok"})

	_, err := client.Approve(t.Context(), accountID, card())
	require.NoError(t, err)
	require.NoError(t, client.ConfirmContact(t.Context(), accountID, "q", domain.Contact{Name: "n", Phone: "p"}))

	require.Equal(t, 2, b.count())
	assert.Equal(t, "/api/bff/approvals/appr_28006196-bd87-4d46-931e-0f74a46b2c57/approve", b.requests[0].URL.Path)
	assert.Equal(t, "/api/bff/contacts/confirm", b.requests[1].URL.Path)
	for _, r := range b.requests {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		assert.Equal(t, account, r.Header.Get("X-Yape-Account-Id"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
	}
}

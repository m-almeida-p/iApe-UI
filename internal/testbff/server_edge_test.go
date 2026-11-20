package testbff_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/testbff"
)

// ask posts a body to /agent of a handler, signed in.
func ask(t *testing.T, h http.Handler, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, h, "/agent", body, headers...)
}

func userSays(text string) string {
	return `{"threadId":"t","runId":"r","messages":[{"role":"user","content":` + quote(text) + `}]}`
}

func TestHandler_TheRunInput(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantText   string // the final text, when it is a stream
		wantRun    bool
	}{
		{name: "invalid JSON", body: `{"messages":`, wantStatus: http.StatusBadRequest},
		{name: "an empty body", body: ``, wantStatus: http.StatusBadRequest},
		{name: "a body that is not an object", body: `[]`, wantStatus: http.StatusBadRequest},
		{name: "JSON followed by more", body: `{} {}`, wantStatus: http.StatusBadRequest},
		{name: "messages that is not a list", body: `{"messages":"hola"}`, wantStatus: http.StatusBadRequest},
		{name: "a content that is not a string", body: `{"messages":[{"role":"user","content":5}]}`, wantStatus: http.StatusBadRequest},
		{name: "the object with nothing in it", body: `{}`, wantStatus: http.StatusCreated, wantText: "You said: ", wantRun: true},
		{name: "messages that is null", body: `{"messages":null}`, wantStatus: http.StatusCreated, wantText: "You said: ", wantRun: true},
		{name: "no messages", body: `{"messages":[]}`, wantStatus: http.StatusCreated, wantText: "You said: ", wantRun: true},
		{
			name:       "only assistant messages: nobody asked",
			body:       `{"messages":[{"role":"assistant","content":"hola"},{"role":"tool","content":"x"}]}`,
			wantStatus: http.StatusCreated, wantText: "You said: ", wantRun: true,
		},
		{
			name:       "the last user message is the one that counts",
			body:       `{"messages":[{"role":"user","content":"primero"},{"role":"assistant","content":"x"},{"role":"user","content":"segundo"},{"role":"assistant","content":"y"}]}`,
			wantStatus: http.StatusCreated, wantText: "You said: segundo", wantRun: true,
		},
		{
			name:       "a blank last user message falls back to an earlier one",
			body:       `{"messages":[{"role":"user","content":"antes"},{"role":"user","content":"  \n\t "},{"role":"user","content":""}]}`,
			wantStatus: http.StatusCreated, wantText: "You said: antes", wantRun: true,
		},
		{
			name:       "the role is case sensitive",
			body:       `{"messages":[{"role":"USER","content":"nope"}]}`,
			wantStatus: http.StatusCreated, wantText: "You said: ", wantRun: true,
		},
		{name: "unknown fields are ignored", body: `{"future":{"a":1},"messages":[{"role":"user","content":"hi","extra":[1]}]}`, wantStatus: http.StatusCreated, wantText: "You said: hi", wantRun: true},
		{name: "unicode is echoed", body: userSays("¿Qué tal? 👋"), wantStatus: http.StatusCreated, wantText: "You said: ¿Qué tal? 👋", wantRun: true},
		{name: "a body over 1 MiB is refused, not played", body: userSays(strings.Repeat("x", 2<<20)), wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := h.Probe().Runs()

			rec := ask(t, h, tc.body)

			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantRun {
				assert.Equal(t, before+1, h.Probe().Runs())
				assert.Contains(t, rec.Body.String(), `"type":"RUN_FINISHED"`)
				var got []string
				for e, err := range decodeBody(rec.Body.String()) {
					require.NoError(t, err)
					if e.Kind == domain.AgentFinal {
						got = append(got, e.Text)
					}
				}
				assert.Equal(t, []string{tc.wantText}, got)
				return
			}
			assert.Equal(t, before, h.Probe().Runs(), "a run that was not understood is not counted")
			assert.Contains(t, rec.Body.String(), `"statusCode":400`)
		})
	}
}

func TestHandler_TheMarkersAreCaseSensitiveAndMatchedAnywhere(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0), testbff.WithSlow(0, 3))
	tests := []struct {
		text      string
		wantInAll string
	}{
		{"#PAY", "You said: #PAY"},
		{"#Pay please", "You said: #Pay please"},
		{"＃pay", "You said: ＃pay"}, // a full-width sharp sign
		{"# pay", "You said: # pay"},
		{"pay", "You said: pay"},
		{"#slow", "tick 3"},
		{"please be #slow now", "tick 3"},
		{"#slowly", "tick 3"},
		{"#Slow", "You said: #Slow"},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			rec := ask(t, h, userSays(tc.text))

			if tc.wantInAll != "" {
				assert.Contains(t, finalOf(t, rec.Body.String()), tc.wantInAll)
			}
		})
	}
}

func finalOf(t *testing.T, sse string) string {
	t.Helper()
	var texts []string
	for e, err := range decodeBody(sse) {
		require.NoError(t, err)
		if e.Kind == domain.AgentFinal {
			texts = append(texts, e.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// kindsOf is what a reply does, in order, by marker: the tool calls, the cards and the text.
func kindsOf(t *testing.T, sse string) (tools, cards []string, text string) {
	t.Helper()
	for e, err := range decodeBody(sse) {
		if err != nil {
			text += "<error:" + err.Error() + ">"
			break
		}
		switch e.Kind {
		case domain.AgentToolCall:
			tools = append(tools, e.Tool)
		case domain.AgentInteraction:
			cards = append(cards, string(e.Interaction.Kind)+":"+e.Interaction.ApprovalID)
		case domain.AgentFinal:
			text += e.Text
		}
	}
	return tools, cards, text
}

func TestHandler_WhenTwoMarkersAreInOneTextTheFirstInTheListWins(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0), testbff.WithSlow(0, 2))
	// The order of the list in the package doc: #error, #slow, #tool, #markdown, #html, #pay, #otp, #contact.
	tests := []struct {
		text      string
		wantTools []string
		wantCards []string
		wantText  string
	}{
		{"#pay #slow", nil, nil, "tick 1 tick 2 "},
		{"#slow #pay", nil, nil, "tick 1 tick 2 "},
		{"#pay #otp", []string{"request_transfer_approval"}, []string{"approval:appr_pay"}, "Review your transfer."},
		{"#otp #pay", []string{"request_transfer_approval"}, []string{"approval:appr_pay"}, "Review your transfer."},
		{"#contact #pay", []string{"request_transfer_approval"}, []string{"approval:appr_pay"}, "Review your transfer."},
		{"#contact #html", nil, nil, htmlStart},
		{"#tool #pay", []string{"lookup"}, nil, "The lookup returned"},
		{"#markdown #tool", []string{"lookup"}, nil, "The lookup returned"},
		{"#html #markdown", nil, nil, "# Report"},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			tools, cards, text := kindsOf(t, ask(t, h, userSays(tc.text)).Body.String())

			assert.Equal(t, tc.wantTools, tools)
			assert.Equal(t, tc.wantCards, cards)
			assert.Contains(t, text, tc.wantText)
		})
	}
	t.Run("#error beats everything: the run fails", func(t *testing.T) {
		rec := ask(t, h, userSays("#pay #slow #error"))

		assert.Contains(t, rec.Body.String(), `"type":"RUN_ERROR"`)
		assert.NotContains(t, rec.Body.String(), "RUN_FINISHED")
		assert.NotContains(t, rec.Body.String(), "ACTION_REQUEST")
	})
}

const htmlStart = "Here is some content."

func decodeBody(sse string) iter.Seq2[domain.AgentEvent, error] {
	return aguibff.Decode(strings.NewReader(sse))
}

func TestHandler_TheRoutes(t *testing.T) {
	h := testbff.NewHandler()
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"GET /agent is not allowed", http.MethodGet, "/agent", http.StatusMethodNotAllowed},
		{"PUT /agent is not allowed", http.MethodPut, "/agent", http.StatusMethodNotAllowed},
		{"DELETE /agent is not allowed", http.MethodDelete, "/agent", http.StatusMethodNotAllowed},
		{"GET /bff/contacts/confirm is not allowed", http.MethodGet, "/bff/contacts/confirm", http.StatusMethodNotAllowed},
		{"GET an approval is not allowed", http.MethodGet, "/bff/approvals/x/approve", http.StatusMethodNotAllowed},
		{"POST /health is not allowed", http.MethodPost, "/health", http.StatusMethodNotAllowed},
		{"HEAD /health is healthy", http.MethodHead, "/health", http.StatusOK},
		{"GET /health is healthy", http.MethodGet, "/health", http.StatusOK},
		{"an unknown route", http.MethodGet, "/nope", http.StatusNotFound},
		{"an unknown action", http.MethodPost, "/bff/approvals/x/cancel", http.StatusNotFound},
		{"an id with a slash in it is another route", http.MethodPost, "/bff/approvals/a/b/approve", http.StatusNotFound},
		{"the root", http.MethodGet, "/", http.StatusNotFound},
		{"/agent/ with a slash", http.MethodPost, "/agent/", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer t")
			req.Header.Set("X-Yape-Account-Id", "a")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
	t.Run("a wrong method says which are allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent", nil))

		assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
	})
	t.Run("a health check needs no token", func(t *testing.T) {
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestHandler_Credentials(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))
	tests := []struct {
		name       string
		auth       *string
		account    *string
		wantStatus int
	}{
		{"a bearer token and an account", new("Bearer abc"), new("acc"), http.StatusCreated},
		{"a token without the Bearer prefix", new("abc"), new("acc"), http.StatusUnauthorized},
		{"the Basic scheme", new("Basic YTpi"), new("acc"), http.StatusUnauthorized},
		{"Bearer with nothing after it", new("Bearer "), new("acc"), http.StatusUnauthorized},
		{"Bearer with only spaces after it", new("Bearer    "), new("acc"), http.StatusUnauthorized},
		{"only the word Bearer", new("Bearer"), new("acc"), http.StatusUnauthorized},
		{"a lowercase scheme is not Bearer", new("bearer abc"), new("acc"), http.StatusUnauthorized},
		{"no Authorization", nil, new("acc"), http.StatusUnauthorized},
		{"no account", new("Bearer abc"), nil, http.StatusUnauthorized},
		{"an empty account", new("Bearer abc"), new(""), http.StatusUnauthorized},
		{"an account of spaces", new("Bearer abc"), new("   "), http.StatusUnauthorized},
		{"nothing", nil, nil, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := h.Probe().Runs()
			req := httptest.NewRequest(http.MethodPost, "/agent", strings.NewReader(userSays("hi")))
			if tc.auth != nil {
				req.Header.Set("Authorization", *tc.auth)
			}
			if tc.account != nil {
				req.Header.Set("X-Yape-Account-Id", *tc.account)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus == http.StatusUnauthorized {
				assert.Contains(t, rec.Body.String(), `"statusCode":401`)
				assert.Equal(t, before, h.Probe().Runs(), "a request that was refused is not a run")
			}
		})
	}
}

func TestHandler_HeaderNamesAreCaseInsensitiveOverTheWire(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	body := userSays("hi")

	_, err = fmt.Fprintf(conn, "POST /agent HTTP/1.1\r\nhost: x\r\nAUTHORIZATION: Bearer abc\r\nx-yape-account-id: acc\r\ncontent-length: %d\r\nCONTENT-TYPE: application/json\r\nconnection: close\r\n\r\n%s", len(body), body)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandler_FiftyRunsAtOnce(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(time.Millisecond), testbff.WithSlow(time.Millisecond, 5))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			text := []string{"#slow", "#pay", "#otp", "#contact", "#tool x", "#markdown", "plain " + fmt.Sprint(i)}[i%7]
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent", strings.NewReader(userSays(text)))
			req.Header.Set("Authorization", "Bearer x")
			req.Header.Set("X-Yape-Account-Id", "a")
			resp, err := http.DefaultClient.Do(req)
			if !assert.NoError(t, err) {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			assert.NoError(t, err)
			assert.Contains(t, string(raw), `"type":"RUN_FINISHED"`, text)
		})
	}
	wg.Wait()

	assert.EqualValues(t, 50, h.Probe().Runs())
	assert.Zero(t, h.Probe().Cancelled())
}

// TestHandler_ACancelledRunIsCountedOncePerDisconnectForEveryMarker leaves each reply after its first event.
func TestHandler_ACancelledRunIsCountedOncePerDisconnectForEveryMarker(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(25*time.Millisecond), testbff.WithSlow(25*time.Millisecond, 400))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	markers := []string{"#slow", "#tool x", "#markdown", "#html", "#pay", "#otp", "#contact", "plain words in a longer text that takes a while to say"}

	for i, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agent", strings.NewReader(userSays(marker)))
			req.Header.Set("Authorization", "Bearer x")
			req.Header.Set("X-Yape-Account-Id", "a")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_, err = bufio.NewReader(resp.Body).ReadString('\n') // it has begun
			require.NoError(t, err)

			cancel() // and the client goes
			_ = resp.Body.Close()

			want := int64(i + 1)
			require.Eventually(t, func() bool { return h.Probe().Cancelled() == want }, 3*time.Second, 5*time.Millisecond)
			time.Sleep(100 * time.Millisecond)
			assert.Equal(t, want, h.Probe().Cancelled(), "once per disconnect: not twice")
			assert.Equal(t, want, h.Probe().Runs())
		})
	}
}

func TestHandler_ARunThatEndsIsNotCancelled(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))

	_ = run(t, h, "hola")
	_ = run(t, h, "#pay")

	assert.EqualValues(t, 2, h.Probe().Runs())
	assert.Zero(t, h.Probe().Cancelled())
}

func TestHandler_WithCutAfter(t *testing.T) {
	tests := []struct {
		name    string
		cut     time.Duration
		wantCut bool
	}{
		{"zero is no cut", 0, false},
		{"negative is no cut", -time.Second, false},
		{"huge is never reached", time.Duration(1<<63 - 1), false},
		{"a short time cuts", 30 * time.Millisecond, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := testbff.NewHandler(testbff.WithSlow(10*time.Millisecond, 20), testbff.WithCutAfter(tc.cut))
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent", strings.NewReader(userSays("#slow")))
			req.Header.Set("Authorization", "Bearer x")
			req.Header.Set("X-Yape-Account-Id", "a")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			assert.Equal(t, tc.wantCut, !strings.Contains(string(raw), "RUN_FINISHED"))
			assert.Zero(t, h.Probe().Cancelled(), "the server cut it off: the client did not leave")
		})
	}
}

func TestHandler_ThePaceNeverPanics(t *testing.T) {
	tests := []struct {
		name string
		opts []testbff.Option
		text string
		want string
	}{
		{"WithSlow(0, 0) says nothing and finishes", []testbff.Option{testbff.WithSlow(0, 0)}, "#slow", ""},
		{"negative chunks say nothing and finish", []testbff.Option{testbff.WithSlow(0, -1)}, "#slow", ""},
		{"very negative chunks", []testbff.Option{testbff.WithSlow(0, -1<<40)}, "#slow", ""},
		{"a negative delay is no delay", []testbff.Option{testbff.WithSlow(-time.Second, 3)}, "#slow", "tick 1 tick 2 tick 3 "},
		{"a negative word delay is no delay", []testbff.Option{testbff.WithWordDelay(-time.Second)}, "a b c", "You said: a b c"},
		{"zero chunks and a delay", []testbff.Option{testbff.WithSlow(time.Hour, 0)}, "#slow", ""},
		{"one chunk with a long delay does not wait", []testbff.Option{testbff.WithSlow(time.Hour, 1)}, "#slow", "tick 1 "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := testbff.NewHandler(tc.opts...)
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent", strings.NewReader(userSays(tc.text)))
			req.Header.Set("Authorization", "Bearer x")
			req.Header.Set("X-Yape-Account-Id", "a")

			resp, err := http.DefaultClient.Do(req)

			require.NoError(t, err, "the handler must not panic: that closes the connection")
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"type":"RUN_FINISHED"`)
			assert.Equal(t, tc.want, finalOf(t, string(raw)))
		})
	}
}

func TestHandler_ConfirmingACode(t *testing.T) {
	h := testbff.NewHandler()
	const route = "/bff/approvals/appr_otp/otp/confirm"
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantIn     string
	}{
		{"the right code", `{"transfer_id":"trf_otp","otp":"123456"}`, http.StatusCreated, "Transfer paid successfully"},
		{"the right code with the run and other fields", `{"transfer_id":"trf_otp","otp":"123456","run_id":"r","extra":{"a":1}}`, http.StatusCreated, "Transfer paid successfully"},
		{"a code of 4 digits", `{"transfer_id":"t","otp":"1234"}`, http.StatusBadRequest, "OTP verification failed"},
		{"a code of 12 digits", `{"transfer_id":"t","otp":"123456123456"}`, http.StatusBadRequest, "OTP verification failed"},
		{"the right code with a space", `{"transfer_id":"t","otp":" 123456"}`, http.StatusBadRequest, "OTP verification failed"},
		{"a code that is a JSON number", `{"transfer_id":"t","otp":123456}`, http.StatusBadRequest, "Missing transfer_id or otp"},
		{"a body that is not JSON", `otp=123456`, http.StatusBadRequest, "Missing transfer_id or otp"},
		{"an empty body", ``, http.StatusBadRequest, "Missing transfer_id or otp"},
		{"an empty code", `{"transfer_id":"t","otp":""}`, http.StatusBadRequest, "Missing transfer_id or otp"},
		{"no transfer", `{"otp":"123456"}`, http.StatusBadRequest, "Missing transfer_id or otp"},
		{"a code with unicode digits", `{"transfer_id":"t","otp":"１２３４５６"}`, http.StatusBadRequest, "OTP verification failed"},
		{"a code of quotes", `{"transfer_id":"t","otp":"12\"34"}`, http.StatusBadRequest, "OTP verification failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h, route, tc.body)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantIn)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		})
	}
}

func TestHandler_AnyApprovalIdCanBeApprovedOrRejected(t *testing.T) {
	h := testbff.NewHandler()

	for _, id := range []string{"appr_unknown", "x", "ñandú", "a%20b", "appr_pay"} {
		approve := post(t, h, "/bff/approvals/"+id+"/approve", `{}`)
		reject := post(t, h, "/bff/approvals/"+id+"/reject", `{}`)

		assert.Equal(t, http.StatusCreated, approve.Code, id)
		assert.Contains(t, approve.Body.String(), `"otp_required":false`, id)
		assert.Equal(t, http.StatusCreated, reject.Code, id)
	}
	t.Run("an approval is answered with its own id", func(t *testing.T) {
		rec := post(t, h, "/bff/approvals/appr_77/approve", ``)

		assert.Contains(t, rec.Body.String(), `"approval_id":"appr_77"`)
	})
	t.Run("only appr_otp asks for a code", func(t *testing.T) {
		rec := post(t, h, "/bff/approvals/appr_otp2/approve", `{}`)

		assert.Contains(t, rec.Body.String(), `"otp_required":false`)
	})
}

func TestHandler_ConfirmingAContact(t *testing.T) {
	h := testbff.NewHandler()
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantIn     string
	}{
		{"all three", `{"query":"Maria","phone":"912345678","name":"Maria Torres"}`, http.StatusCreated, `"status":"ok"`},
		{"unicode and quotes", `{"query":"Ma\"ría","phone":"+51 9","name":"李小龍 🥋"}`, http.StatusCreated, `"status":"ok"`},
		{"a body that is not JSON", `query=Maria`, http.StatusBadRequest, "bad body"},
		{"an empty body", ``, http.StatusBadRequest, "bad body"},
		{"a list", `[]`, http.StatusBadRequest, "bad body"},
		{"a field that is not a string", `{"query":"M","phone":912345678,"name":"x"}`, http.StatusBadRequest, "bad body"},
		{"nothing", `{}`, http.StatusBadRequest, "Missing required fields: query, phone, name"},
		{"empty fields", `{"query":"","phone":"","name":""}`, http.StatusBadRequest, "Missing required fields: query, phone, name"},
		{"only the phone is missing", `{"query":"M","name":"x"}`, http.StatusBadRequest, "Missing required fields: phone"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h, "/bff/contacts/confirm", tc.body)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantIn)
		})
	}
}

func TestHandler_ActionsNeedTheCredentialsToo(t *testing.T) {
	h := testbff.NewHandler()
	for _, path := range []string{"/bff/approvals/x/approve", "/bff/approvals/x/reject", "/bff/approvals/x/otp/confirm", "/bff/contacts/confirm"} {
		for _, auth := range []string{"", "Bearer ", "Token abc"} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("X-Yape-Account-Id", "a")
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s with %q", path, auth)
		}
	}
}

func TestHandler_TheStreamIsServerSentEvents(t *testing.T) {
	h := testbff.NewHandler(testbff.WithWordDelay(0))

	rec := ask(t, h, userSays("hola"))

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	for block := range strings.SplitSeq(strings.TrimSpace(rec.Body.String()), "\n\n") {
		assert.True(t, strings.HasPrefix(block, "data: {"), "every event is one data line: %q", block)
		assert.NotContains(t, block, "\n")
	}
	assert.True(t, strings.HasSuffix(rec.Body.String(), "\n\n"))
}

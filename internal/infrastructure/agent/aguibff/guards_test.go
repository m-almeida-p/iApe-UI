package aguibff_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/infrastructure/config"
)

// deadlines is a transport that writes down the time limit each request was given.
type deadlines struct {
	mu   sync.Mutex
	left []time.Duration
	next http.RoundTripper
}

func (d *deadlines) RoundTrip(r *http.Request) (*http.Response, error) {
	left := time.Duration(-1)
	if dl, ok := r.Context().Deadline(); ok {
		left = time.Until(dl)
	}
	d.mu.Lock()
	d.left = append(d.left, left)
	d.mu.Unlock()
	return d.next.RoundTrip(r)
}

func (d *deadlines) last() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.left[len(d.left)-1]
}

// An answer to a card must come back before its claim goes stale (domain.ClaimTTL): else another click takes the card over while the
// first call is still out. So a card's action is given at most half of it, whatever the time limit of the settings.
func TestActions_AreGivenLessTimeThanTheClaimLasts(t *testing.T) {
	ceiling := domain.ClaimTTL / 2
	for name, tc := range map[string]struct {
		setting time.Duration
		want    time.Duration
	}{
		"a limit longer than the claim is cut":   {time.Hour, ceiling},
		"the limit that equals the claim is cut": {domain.ClaimTTL, ceiling},
		"no limit at all is cut too":             {0, ceiling},
		"a shorter limit is kept":                {10 * time.Second, 10 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBFF(t, serveJSON(t, http.StatusCreated, "approve-ok.json"))
			rec := &deadlines{next: http.DefaultTransport}
			c := aguibff.NewClient(config.Agent{BaseURL: b.srv.URL, RequestTimeout: tc.setting}, &fakeTokens{token: "t"}, &http.Client{Transport: rec})

			for _, call := range []func() error{
				func() error { _, err := c.Approve(t.Context(), accountID, card()); return err },
				func() error { _, err := c.Reject(t.Context(), accountID, card()); return err },
				func() error { _, err := c.ConfirmOTP(t.Context(), accountID, card(), "123456"); return err },
			} {
				require.NoError(t, call())

				assert.LessOrEqual(t, rec.last(), tc.want)
				assert.Greater(t, rec.last(), tc.want-5*time.Second)
			}
		})
	}

	t.Run("a turn of the agent keeps the limit of the settings", func(t *testing.T) {
		b := newBFF(t, serveFixture(t, "guard.sse"))
		rec := &deadlines{next: http.DefaultTransport}
		c := aguibff.NewClient(config.Agent{BaseURL: b.srv.URL, RequestTimeout: time.Hour}, &fakeTokens{token: "t"}, &http.Client{Transport: rec})

		_, err := collect(c.Run(t.Context(), turn("hola")))

		require.NoError(t, err)
		assert.Greater(t, rec.last(), domain.ClaimTTL, "a turn is not an answer to a card")
	})
}

// What the BFF says about the transfer is read: a 2xx that does not say it was done is not taken for paid.
func TestApprove_ANotConfirmedTransferIsNotPaid(t *testing.T) {
	for name, body := range map[string]string{
		"success is false":         `{"success":false,"otp_required":false,"displayMessage":"Transfer paid successfully."}`,
		"the execution failed":     `{"success":true,"otp_required":false,"execution":{"status":"FAILED"}}`,
		"the execution is pending": `{"success":true,"otp_required":false,"execution":{"status":"PENDING"}}`,
		"the status is blank text": `{"success":true,"otp_required":false,"execution":{"status":"  "}}`,
		"the status in other case": `{"success":true,"otp_required":false,"execution":{"status":"failed"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			})

			got, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrAgentUnavailable, "the service did not confirm: the page says the answer is not known")
			assert.NotErrorIs(t, err, domain.ErrNotSent, "the transfer may have gone through: the card stays claimed")
			assert.Equal(t, domain.ActionResult{}, got)
		})
	}

	for name, body := range map[string]string{
		"a confirmed transfer":               `{"success":true,"otp_required":false,"displayMessage":"ok","execution":{"status":"SUCCESS"}}`,
		"the status in other case":           `{"success":true,"otp_required":false,"displayMessage":"ok","execution":{"status":"success"}}`,
		"no word about success at all":       `{"otp_required":false,"displayMessage":"ok"}`,
		"a transfer that waits for its code": `{"success":true,"otp_required":true,"transfer_id":"trf_1","displayMessage":"ok"}`,
	} {
		t.Run(name+" goes through", func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			})

			got, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

			require.NoError(t, err)
			assert.Equal(t, "ok", got.Message)
		})
	}
}

func TestConfirmOTP_ANotConfirmedTransferIsNotPaid(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"success":false,"displayMessage":"Transfer paid successfully."}`)
	})

	_, err := clientFor(b, &fakeTokens{token: "t"}).ConfirmOTP(t.Context(), accountID, card(), "123456")

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.NotErrorIs(t, err, domain.ErrNotSent)
}

// A redirect is not followed on the calls that carry the account's token and the user's code: the BFF is at the address of the
// settings and nowhere else.
func TestClient_DoesNotFollowRedirects(t *testing.T) {
	elsewhere := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusCreated) })
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/bff/approvals/x/otp/confirm", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)
	c := aguibff.NewClient(config.Agent{BaseURL: redirecting.URL, RequestTimeout: 5 * time.Second}, &fakeTokens{token: "secret-token"}, nil)

	_, otpErr := c.ConfirmOTP(t.Context(), accountID, card(), "123456")
	_, runErr := collect(c.Run(t.Context(), turn("hola")))

	require.Error(t, otpErr)
	require.Error(t, runErr)
	assert.Zero(t, elsewhere.count(), "neither the code nor the token went anywhere else")
}

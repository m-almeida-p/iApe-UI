package aguibff_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

// The BFF explains a refusal in "message" (NestJS) or in "error" (the approvals' own); a repeat of an answer is refused with what the
// approval is already: that is how a card whose answer was lost finds out what became of it.
func TestRefusal_WhatTheBFFSaysAndWhatTheApprovalAlreadyIs(t *testing.T) {
	for name, tc := range map[string]struct{ body, message, resolved string }{
		"the approval's own key":          {`{"error":"approval already resolved: APPROVED"}`, "approval already resolved: APPROVED", "APPROVED"},
		"the NestJS key":                  {`{"message":"approval already resolved: REJECTED","statusCode":400}`, "approval already resolved: REJECTED", "REJECTED"},
		"any state it names":              {`{"error":"approval already resolved: EXPIRED"}`, "approval already resolved: EXPIRED", "EXPIRED"},
		"a refusal that is not one":       {`{"error":"OTP verification failed"}`, "OTP verification failed", ""},
		"the text of a lookalike":         {`{"error":"the approval already resolved: APPROVED, they say"}`, "the approval already resolved: APPROVED, they say", ""},
		"message wins when both exist":    {`{"message":"from message","error":"from error"}`, "from message", ""},
		"an empty message falls to error": {`{"message":"","error":"approval already resolved: APPROVED"}`, "approval already resolved: APPROVED", "APPROVED"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			})

			_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), card())

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, tc.message, refused.Message)
			assert.Equal(t, tc.resolved, refused.Resolved)
		})
	}

	t.Run("an error that is not text is not a refusal the user can read", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":5}}`))
		})

		_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), card())

		var refused *domain.ActionError
		assert.NotErrorAs(t, err, &refused)
		require.Error(t, err)
	})
}

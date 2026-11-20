package aguibff_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/infrastructure/config"
)

// What a failed answer lets the caller know: whether the request certainly did not reach the service (then the card can be given back,
// nothing was done) or may have (then it cannot: a payment may have gone through).

func TestNotSent_WhatNeverLeftTheComputerIsSaidSo(t *testing.T) {
	t.Run("a token program that fails", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusCreated) })

		_, err := clientFor(b, &fakeTokens{err: errors.New("no VPN")}).Approve(t.Context(), accountID, card())

		require.ErrorIs(t, err, domain.ErrNotSent)
		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Zero(t, b.count(), "and nothing was sent")
	})
	t.Run("a token that is empty", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusCreated) })

		_, err := clientFor(b, &fakeTokens{token: "  "}).Reject(t.Context(), accountID, card())

		require.ErrorIs(t, err, domain.ErrNotSent)
		assert.Zero(t, b.count())
	})
	t.Run("a service nobody listens at", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close()) // the port is free now: the connection is refused
		c := aguibff.NewClient(config.Agent{BaseURL: "http://" + addr, RequestTimeout: 2 * time.Second}, &fakeTokens{token: "t"}, nil)

		_, err = c.ConfirmOTP(t.Context(), accountID, card(), "123456")

		require.ErrorIs(t, err, domain.ErrNotSent)
		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	})
	t.Run("a token that expired and cannot be renewed", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusUnauthorized) })

		_, err := clientFor(b, &fakeTokens{token: "old", err: errors.New("refresh failed")}).Approve(t.Context(), accountID, card())

		require.ErrorIs(t, err, domain.ErrNotSent, "a 401 is a request the service did not act on")
	})
	t.Run("the same for a contact and for a turn", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusCreated) })
		c := clientFor(b, &fakeTokens{err: errors.New("no VPN")})

		contactErr := c.ConfirmContact(t.Context(), accountID, "Maria", domain.Contact{Name: "Maria Torres", Phone: "912345678"})
		_, runErr := collect(c.Run(t.Context(), turn("hola")))

		require.ErrorIs(t, contactErr, domain.ErrNotSent)
		require.ErrorIs(t, runErr, domain.ErrNotSent)
	})
}

func TestNotSent_WhatMayHaveReachedTheServiceIsNeverSaidNotSent(t *testing.T) {
	t.Run("a service that does not answer in time", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) { <-r.Context().Done() })
		c := aguibff.NewClient(config.Agent{BaseURL: b.srv.URL, RequestTimeout: 100 * time.Millisecond}, &fakeTokens{token: "t"}, nil)

		_, err := c.Approve(t.Context(), accountID, card())

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotSent, "the payment may have been made")
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("a connection that is dropped after the request was read", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
		})

		_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotSent)
	})
	t.Run("an answer of the gateway", func(t *testing.T) {
		for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError} {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(status) })

			_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

			require.Error(t, err, "%d", status)
			assert.NotErrorIs(t, err, domain.ErrNotSent, "%d: the service was reached", status)
		}
	})
	t.Run("a refusal is the service's answer, not a request that was not sent", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"OTP verification failed"}`))
		})

		_, err := clientFor(b, &fakeTokens{token: "t"}).ConfirmOTP(t.Context(), accountID, card(), "000000")

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.NotErrorIs(t, err, domain.ErrNotSent)
	})
	t.Run("a body that cannot be read after the service answered", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`not json`))
		})

		_, err := clientFor(b, &fakeTokens{token: "t"}).Approve(t.Context(), accountID, card())

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotSent)
	})
}

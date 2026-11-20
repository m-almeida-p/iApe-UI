package history_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/history"
	"iape-ui/internal/core/port/resource/mocks"
)

var user = uuid.New()

func rig(t *testing.T) (*history.Service, *mocks.HistoryGateway, *bytes.Buffer) {
	t.Helper()
	gw := mocks.NewHistoryGateway(t)
	logs := &bytes.Buffer{}
	return history.NewService(history.Dependencies{History: gw, Logger: slog.New(slog.NewTextHandler(logs, nil))}), gw, logs
}

func TestConversations_AreThoseOfTheAccountOfTheUser(t *testing.T) {
	svc, gw, _ := rig(t)
	page := domain.PageRequest{Before: "123~conv_v1_x", Size: 20}
	want := domain.ConversationList{Conversations: []domain.ConversationSummary{{ID: "conv_v1_a", FirstMessage: "hola"}}, Older: "9~y"}
	gw.On("Conversations", mock.Anything, user, page).Return(want, nil).Once()

	got, err := svc.Conversations(t.Context(), user, page)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTranscript_IsOfTheConversationForTheAccountOfTheUser(t *testing.T) {
	svc, gw, _ := rig(t)
	want := domain.Transcript{ConversationID: "conv_v1_a", Messages: []domain.TranscriptMessage{{ID: "1", Role: domain.RoleUser, Text: "hola"}}}
	gw.On("Messages", mock.Anything, user, "conv_v1_a", domain.PageRequest{}).Return(want, nil).Once()

	got, err := svc.Transcript(t.Context(), user, "conv_v1_a", domain.PageRequest{})

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestErrorsAreTheGatewaysOwn(t *testing.T) {
	for _, failure := range []error{domain.ErrNotFound, domain.ErrInvalidPage, domain.ErrHistoryUnavailable, errors.New("other")} {
		t.Run(failure.Error(), func(t *testing.T) {
			svc, gw, _ := rig(t)
			gw.On("Conversations", mock.Anything, user, mock.Anything).Return(domain.ConversationList{}, failure).Once()
			gw.On("Messages", mock.Anything, user, "c", mock.Anything).Return(domain.Transcript{}, failure).Once()

			_, errList := svc.Conversations(t.Context(), user, domain.PageRequest{})
			_, errTranscript := svc.Transcript(t.Context(), user, "c", domain.PageRequest{})

			assert.ErrorIs(t, errList, failure)
			assert.ErrorIs(t, errTranscript, failure)
		})
	}
}

func TestAnOutageIsLoggedAndAMistakeIsNot(t *testing.T) {
	svc, gw, logs := rig(t)
	gw.On("Conversations", mock.Anything, user, mock.Anything).Return(domain.ConversationList{}, domain.ErrNotFound).Once()
	_, _ = svc.Conversations(t.Context(), user, domain.PageRequest{})
	assert.Empty(t, logs.String(), "a conversation that is not there is not a fault")

	gw.On("Messages", mock.Anything, user, "c", mock.Anything).Return(domain.Transcript{}, fmt.Errorf("%w: down", domain.ErrHistoryUnavailable)).Once()
	_, _ = svc.Transcript(t.Context(), user, "c", domain.PageRequest{})
	assert.Contains(t, logs.String(), "reading a conversation")
	assert.NotContains(t, logs.String(), user.String(), "the account is not in the log")
}

func TestACallerThatLeftIsNotAFault(t *testing.T) {
	svc, gw, logs := rig(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gw.On("Conversations", mock.Anything, user, mock.Anything).Return(domain.ConversationList{}, fmt.Errorf("%w: gone", domain.ErrHistoryUnavailable)).Once()

	_, _ = svc.Conversations(ctx, user, domain.PageRequest{})

	assert.Empty(t, logs.String())
}

func TestNewServiceHasALogger(t *testing.T) {
	assert.NotNil(t, history.NewService(history.Dependencies{History: mocks.NewHistoryGateway(t)}))
}

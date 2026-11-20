// The history adapter in front of the real history API (ms-ml-conv-history-bs). Skipped unless the environment or .env says
// HISTORY_LIVE=1, because it needs that service running with data of the account.
//
//	HISTORY_LIVE=1 go test ./it/live/ -run TestLiveHistory
//
// The history is HISTORY_BASE_URL and the account YAPE_ACCOUNT_ID, both read from the environment or .env.
package live_test

import (
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/config"
	"iape-ui/internal/infrastructure/historyapi"
	"iape-ui/internal/platform/envcfg"
)

func TestLiveHistory(t *testing.T) {
	lookup, err := envcfg.EnvLookup("../../.env")
	require.NoError(t, err)
	if live, _ := lookup("HISTORY_LIVE"); live != "1" {
		t.Skip("set HISTORY_LIVE=1 (with HISTORY_BASE_URL and YAPE_ACCOUNT_ID) to read the real history")
	}
	base, _ := lookup("HISTORY_BASE_URL")
	accountText, _ := lookup("YAPE_ACCOUNT_ID")
	require.NotEmpty(t, base, "HISTORY_BASE_URL is not set")
	account, err := uuid.Parse(accountText)
	require.NoError(t, err, "YAPE_ACCOUNT_ID is not a UUID")
	cfg, err := config.Load(func(key string) (string, bool) {
		switch key {
		case "DB_URL":
			return "postgres://u:p@localhost/db", true
		case "AGENT_BASE_URL":
			return "http://localhost:4000", true
		case "YAPE_ACCOUNT_ID":
			return accountText, true
		case "HISTORY_BASE_URL":
			return base, true
		}
		return "", false
	})
	require.NoError(t, err)
	client := historyapi.NewClient(cfg.History, nil)

	list, err := client.Conversations(t.Context(), account, domain.PageRequest{Size: 5})
	require.NoError(t, err)
	require.NotEmpty(t, list.Conversations, "the account has no conversation in the history")
	for _, c := range list.Conversations {
		assert.NotEmpty(t, c.ID)
		assert.NotEmpty(t, c.Cursor)
		assert.False(t, c.LastAt.IsZero())
	}
	assert.Empty(t, list.Newer, "the first page has no newer one")

	t.Run("the next page is the older one", func(t *testing.T) {
		if list.Older == "" {
			t.Skip("the account has one page of conversations")
		}
		older, err := client.Conversations(t.Context(), account, domain.PageRequest{Before: list.Older, Size: 5})

		require.NoError(t, err)
		require.NotEmpty(t, older.Conversations)
		assert.True(t, older.Conversations[0].LastAt.Before(list.Conversations[len(list.Conversations)-1].LastAt.Add(1)),
			"older conversations come after the newer ones")
		assert.NotEqual(t, list.Conversations[0].ID, older.Conversations[0].ID)
	})

	t.Run("a conversation is read, and by its own account only", func(t *testing.T) {
		id := list.Conversations[0].ID

		got, err := client.Messages(t.Context(), account, id, domain.PageRequest{})
		require.NoError(t, err)
		require.NotEmpty(t, got.Messages)
		assert.Equal(t, id, got.ConversationID)
		assert.Equal(t, domain.RoleUser, got.Messages[0].Role, "a chat starts with what the user wrote")

		_, err = client.Messages(t.Context(), uuid.MustParse("5e565f30-3320-4c95-8080-a56cf8f3eb2e"), id, domain.PageRequest{})
		assert.ErrorIs(t, err, domain.ErrNotFound, "another account does not see it, and is not told it exists")

		_, err = client.Messages(t.Context(), account, "conv_v1_00000000-0000-4000-8000-000000000000", domain.PageRequest{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a long conversation is read in pages", func(t *testing.T) {
		var longest domain.ConversationSummary
		for _, c := range list.Conversations {
			if c.MessageCount > longest.MessageCount {
				longest = c
			}
		}
		if longest.MessageCount < 3 {
			t.Skip("no conversation with three messages")
		}

		latest, err := client.Messages(t.Context(), account, longest.ID, domain.PageRequest{Size: 2})
		require.NoError(t, err)
		require.NotEmpty(t, latest.Older, "there are older messages")
		older, err := client.Messages(t.Context(), account, longest.ID, domain.PageRequest{Before: latest.Older, Size: 2})
		require.NoError(t, err)

		require.NotEmpty(t, older.Messages)
		assert.Less(t, older.Messages[len(older.Messages)-1].Seq, latest.Messages[0].Seq, "older messages come first")
	})

	t.Run("a cursor of another kind is refused", func(t *testing.T) {
		_, err := client.Conversations(t.Context(), account, domain.PageRequest{Before: "not-a-cursor"})

		assert.ErrorIs(t, err, domain.ErrInvalidPage)
	})
}

package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

// at is a time of the day of the clock of the page, in UTC; Lima is five hours behind.
func at(hour, min int) time.Time { return time.Date(2026, 11, 20, hour, min, 0, 0, time.UTC) }

func summary(id, first string, count int, last time.Time) domain.ConversationSummary {
	return domain.ConversationSummary{ID: id, Cursor: "c~" + id, MessageCount: count, FirstAt: last.Add(-time.Minute), LastAt: last, FirstMessage: first}
}

func TestHistoryList(t *testing.T) {
	h := newChatHarness(t)
	list := domain.ConversationList{
		Conversations: []domain.ConversationSummary{
			summary("conv_v1_a", "quiero yapear", 5, at(17, 5)),                          // 12:05 in Lima, today
			summary("conv_v1_b", "<b>negrita</b> & más", 1, at(1, 30).AddDate(0, 0, -3)), // 20:30 in Lima, the 16th
			summary("conv_v1_c", "", 2, at(2, 0).AddDate(-1, 0, 0)),                      // last year
		},
		Older: "c~conv_v1_c",
	}
	h.history.On("Conversations", mock.Anything, alice.ID, domain.PageRequest{Size: 30}).Return(list, nil).Once()

	rec := h.do(http.MethodGet, "/history?active=conv_v1_a", nil, htmx)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	root := parseHTML(t, rec.Body.String())
	rows := byTestID(root, "conversation")
	require.Len(t, rows, 3)

	t.Run("each row is a link to its conversation, with what the user first wrote", func(t *testing.T) {
		links := byTestID(root, "conversation-link")
		assert.Equal(t, "/c/conv_v1_a", attrOf(links[0], "href"))
		assert.Equal(t, "conv_v1_a", attrOf(rows[0], "data-id"))
		assert.Contains(t, textContent(links[0]), "quiero yapear")
	})
	t.Run("the conversation on screen is marked", func(t *testing.T) {
		assert.Equal(t, "page", attrOf(byTestID(root, "conversation-link")[0], "aria-current"))
		assert.Contains(t, attrOf(rows[0], "class"), "active")
		assert.False(t, hasAttr(byTestID(root, "conversation-link")[1], "aria-current"))
	})
	t.Run("the hours are those of Lima, and the day only when it is not today", func(t *testing.T) {
		meta := byTestID(root, "conversation-meta")
		assert.Equal(t, "12:05 · 5 messages", textContent(meta[0]))
		assert.Equal(t, "16 Nov 20:30 · 1 message", textContent(meta[1]))
		assert.Equal(t, "19 Nov 2025 21:00 · 2 messages", textContent(meta[2]))
	})
	t.Run("what the history says is text, never markup", func(t *testing.T) {
		assert.Empty(t, elementsOf(root, "b"))
		assert.Contains(t, textContent(byTestID(root, "conversation-link")[1]), "<b>negrita</b> & más")
	})
	t.Run("a conversation with no first message says so", func(t *testing.T) {
		assert.Contains(t, textContent(byTestID(root, "conversation-link")[2]), "(no message)")
	})
	t.Run("the older conversations are one button away, which adds to the list", func(t *testing.T) {
		more := byTestID(root, "history-more")
		require.Len(t, more, 1)
		assert.Equal(t, "/history?active=conv_v1_a&before=c~conv_v1_c", attrOf(more[0], "hx-get"))
		assert.Equal(t, "#history-list", attrOf(more[0], "hx-target"))
		assert.Equal(t, "beforeend", attrOf(more[0], "hx-swap"))
	})
	t.Run("the page never shows the account of the user", func(t *testing.T) {
		assert.NotContains(t, rec.Body.String(), alice.ID.String())
		assert.NotContains(t, rec.Body.String(), "3100")
	})
}

func TestHistoryList_AnOlderPageIsOnlyWhatToAdd(t *testing.T) {
	h := newChatHarness(t)
	list := domain.ConversationList{Conversations: []domain.ConversationSummary{summary("conv_v1_z", "viejo", 3, at(10, 0).AddDate(0, -2, 0))}}
	h.history.On("Conversations", mock.Anything, alice.ID, domain.PageRequest{Before: "c~conv_v1_c", Size: 30}).Return(list, nil).Once()

	rec := h.do(http.MethodGet, "/history?before=c~conv_v1_c", nil, htmx)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())
	assert.Len(t, byTestID(root, "conversation"), 1)
	assert.Empty(t, byTestID(root, "conversation-list"), "the list is already on the page")
	assert.NotContains(t, rec.Body.String(), `data-testid="history-more"`, "no more pages: the button goes")
	assert.Contains(t, rec.Body.String(), `id="history-more"`)
	assert.Contains(t, rec.Body.String(), `hx-swap-oob="true"`)
}

func TestHistoryList_NoConversationsYet(t *testing.T) {
	h := newChatHarness(t)
	h.history.On("Conversations", mock.Anything, alice.ID, mock.Anything).Return(domain.ConversationList{}, nil).Once()

	rec := h.do(http.MethodGet, "/history", nil, htmx)

	assert.Len(t, byTestID(parseHTML(t, rec.Body.String()), "history-empty"), 1)
}

func TestHistoryList_AHistoryThatIsDownSaysSoWithAWayToTryAgain(t *testing.T) {
	h := newChatHarness(t)
	h.history.On("Conversations", mock.Anything, alice.ID, domain.PageRequest{Size: 30}).Return(domain.ConversationList{}, fmt.Errorf("%w: down", domain.ErrHistoryUnavailable)).Once()

	rec := h.do(http.MethodGet, "/history?active=conv_v1_a", nil, htmx)

	assert.Equal(t, http.StatusOK, rec.Code, "htmx swaps only what answers well: the list has to say why it is not there")
	root := parseHTML(t, rec.Body.String())
	assert.Contains(t, textContent(byTestID(root, "history-error")[0]), "not available")
	assert.Equal(t, "/history?active=conv_v1_a", attrOf(byTestID(root, "history-retry")[0], "hx-get"))
	assert.NotContains(t, rec.Body.String(), "down", "what went wrong stays in the log")
}

func TestHistoryList_Refusals(t *testing.T) {
	t.Run("a cursor that is not the history's is a 400", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Conversations", mock.Anything, alice.ID, domain.PageRequest{Before: "x y", Size: 30}).Return(domain.ConversationList{}, domain.ErrInvalidPage).Once()

		assert.Equal(t, http.StatusBadRequest, h.do(http.MethodGet, "/history?before=x+y", nil, htmx).Code)
	})
	t.Run("another failure is a 500 that says nothing", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Conversations", mock.Anything, alice.ID, mock.Anything).Return(domain.ConversationList{}, fmt.Errorf("secret detail")).Once()

		rec := h.do(http.MethodGet, "/history", nil, htmx)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret")
	})
	t.Run("without a history there is no Historial", func(t *testing.T) {
		h := newHarnessWith(t, Options{}, false)

		assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, "/history", nil, htmx).Code)
	})
	t.Run("without a session it is the sign-in page", func(t *testing.T) {
		h := newChatHarness(t)
		rec := httptest.NewRecorder()
		h.route.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/history", nil))

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Contains(t, rec.Header().Get("Location"), "/login")
	})
}

func TestHistoryList_EachUserSeesTheirOwn(t *testing.T) {
	h := newChatHarness(t)
	h.history.On("Conversations", mock.Anything, alice.ID, mock.Anything).Return(domain.ConversationList{Conversations: []domain.ConversationSummary{summary("conv_v1_ana", "mío", 1, at(17, 0))}}, nil).Once()

	rec := h.do(http.MethodGet, "/history", nil, htmx)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "mío")
	h.history.AssertNumberOfCalls(t, "Conversations", 1)
	h.history.AssertCalled(t, "Conversations", mock.Anything, alice.ID, mock.Anything)
}

func TestRowTitle(t *testing.T) {
	long := strings.Repeat("ñ", 100)
	tests := []struct {
		name string
		in   domain.ConversationSummary
		want string
	}{
		{"as it is", domain.ConversationSummary{FirstMessage: "hola"}, "hola"},
		{"one line", domain.ConversationSummary{FirstMessage: "  hola \n\n  mundo\t!  "}, "hola mundo !"},
		{"cut at 90 characters, not bytes", domain.ConversationSummary{FirstMessage: long}, strings.Repeat("ñ", 90) + "…"},
		{"cut by the history", domain.ConversationSummary{FirstMessage: "hola", FirstMessageTruncated: true}, "hola…"},
		{"nothing", domain.ConversationSummary{}, "(no message)"},
		{"spaces only", domain.ConversationSummary{FirstMessage: " \n "}, "(no message)"},
		{"direction tricks are removed", domain.ConversationSummary{FirstMessage: "Rosa\u202e 123"}, "Rosa 123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rowTitle(tc.in))
		})
	}
}

func TestHistoryURL(t *testing.T) {
	assert.Equal(t, "/history", historyURL("", ""))
	assert.Equal(t, "/history?active=conv_v1_a", historyURL("conv_v1_a", ""))
	assert.Equal(t, "/history?before=c~x", historyURL("", "c~x"))
	assert.Equal(t, "/history?active=a+b%26c&before=1%262", historyURL("a b&c", "1&2"), "what goes in the query is encoded")
}

func TestShortTime(t *testing.T) {
	h := newChatHarness(t) // 13:00 of 2026-11-20 in Lima
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"no time", time.Time{}, ""},
		{"today", time.Date(2026, 11, 20, 15, 4, 0, 0, time.UTC), "10:04"},
		{"the start of today in Lima, which is yesterday in UTC's eyes", time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC), "00:00"},
		{"the evening before, which UTC already calls today", time.Date(2026, 11, 20, 4, 59, 0, 0, time.UTC), "19 Nov 23:59"},
		{"this year", time.Date(2026, 3, 2, 17, 5, 0, 0, time.UTC), "2 Mar 12:05"},
		{"another year", time.Date(2025, 12, 31, 3, 0, 0, 0, time.UTC), "30 Dec 2025 22:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, h.app.shortTime(tc.at))
		})
	}
}

package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/service"
	"iape-ui/internal/core/port/service/mocks"
)

type chatHarness struct {
	t       *testing.T
	auth    *mocks.Auth
	chat    *mocks.Chat
	history *mocks.History
	cards   *mocks.Interactions
	app     *App
	route   http.Handler
}

// clock is the time of the page: the same day for every test, so the hours of the history do not depend on the day they run.
var clock = time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC) // 13:00 in Lima

func newChatHarness(t *testing.T) *chatHarness {
	t.Helper()
	return newHarnessWith(t, Options{}, true)
}

func newChatHarnessWithOptions(t *testing.T, opts Options) *chatHarness {
	t.Helper()
	return newHarnessWith(t, opts, true)
}

// newHarnessWith makes the page with the options and, when withHistory, a history to read.
func newHarnessWith(t *testing.T, opts Options, withHistory bool) *chatHarness {
	t.Helper()
	lima, err := time.LoadLocation("America/Lima")
	require.NoError(t, err)
	if opts.Now == nil {
		opts.Now = func() time.Time { return clock }
	}
	if opts.Location == nil {
		opts.Location = lima
	}
	h := &chatHarness{t: t, auth: mocks.NewAuth(t), chat: mocks.NewChat(t), history: mocks.NewHistory(t), cards: mocks.NewInteractions(t)}
	h.auth.On("Authenticate", mock.Anything, "tok").Return(alice, nil).Maybe()
	svc := Services{Auth: h.auth, Chat: h.chat, Interactions: h.cards}
	if withHistory {
		svc.History = h.history
	}
	h.app = testAppWith(t, svc, opts)
	h.route = h.app.Routes()
	return h
}

func (h *chatHarness) do(method, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var req *http.Request
	if form != nil {
		req = postForm(path, form, headers)
		req.Method = method
	} else {
		req = httptest.NewRequest(method, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	h.route.ServeHTTP(rec, req)
	return rec
}

const convID = "conv_v1_11111111-1111-4111-8111-111111111111"

var htmx = map[string]string{"HX-Request": "true"}

// live makes a message that is live in this server.
func live(role domain.Role, status domain.Status, content string) domain.Message {
	return domain.Message{ID: uuid.New(), ConversationID: convID, Role: role, Status: status, Content: content}
}

func history(msgs ...domain.TranscriptMessage) domain.Transcript {
	return domain.Transcript{ConversationID: convID, Messages: msgs}
}

func said(id string, role domain.Role, text string) domain.TranscriptMessage {
	return domain.TranscriptMessage{ID: id, Seq: 1, Role: role, Text: text}
}

func TestNewChatPage(t *testing.T) {
	h := newChatHarness(t)
	h.chat.On("NewConversation", alice.ID).Return(convID).Once()

	rec := h.do(http.MethodGet, "/", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())

	t.Run("offers the composer, which posts to the id the chat will have", func(t *testing.T) {
		input := byTestID(root, "composer-input")
		require.Len(t, input, 1)
		assert.Equal(t, "content", attrOf(input[0], "name"))
		assert.False(t, hasAttr(input[0], "maxlength"), "a long paste is not cut off without a word: the server says what the limit is")
		for _, f := range elementsOf(root, "form") {
			if attrOf(f, "id") == "composer" {
				assert.Equal(t, "/c/"+convID+"/messages", attrOf(f, "hx-post"))
				assert.Equal(t, "/c/"+convID+"/messages", attrOf(f, "action"))
				assert.Equal(t, "#messages", attrOf(f, "hx-target"))
				return
			}
		}
		t.Fatal("no composer")
	})
	t.Run("has a way to start over", func(t *testing.T) {
		assert.Len(t, byTestID(root, "new-chat"), 1)
	})
	t.Run("shows the empty state and no messages", func(t *testing.T) {
		assert.Empty(t, byTestID(root, "message"))
		assert.Contains(t, rec.Body.String(), "How can I help?")
	})
	t.Run("the input has a label, and Stop is there for the streaming", func(t *testing.T) {
		assert.Contains(t, rec.Body.String(), `<label for="composer-input" class="visually-hidden">Message</label>`)
		assert.True(t, hasAttr(byTestID(root, "stop")[0], "hidden"))
	})
	t.Run("the Historial is loaded by the page, with the chat on screen", func(t *testing.T) {
		list := byTestID(root, "history")
		require.Len(t, list, 1)
		assert.Equal(t, "/history?active="+convID, attrOf(list[0], "hx-get"))
		assert.Contains(t, attrOf(list[0], "hx-trigger"), "history:refresh")
		assert.Equal(t, "Historial", textContent(byTestID(root, "history-title")[0]))
	})
}

func TestNewChatPage_NoHistoryNoHistorial(t *testing.T) {
	h := newHarnessWith(t, Options{}, false)
	h.chat.On("NewConversation", alice.ID).Return(convID).Once()

	rec := h.do(http.MethodGet, "/", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())
	assert.Empty(t, byTestID(root, "history"))
	assert.Empty(t, byTestID(root, "history-title"))
	assert.Len(t, byTestID(root, "composer-input"), 1, "the chat goes on without a history")
}

func TestConversationPage(t *testing.T) {
	h := newChatHarness(t)
	user := said("1", domain.RoleUser, "What is **2+2**?\n<script>alert(1)</script>")
	reply := said("2", domain.RoleAssistant, "It is **4**.")
	h.history.On("Transcript", mock.Anything, alice.ID, convID, domain.PageRequest{Size: 100}).Return(history(user, reply), nil).Once()

	rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())
	messages := byTestID(root, "message")
	require.Len(t, messages, 2)

	t.Run("the composer posts to this conversation", func(t *testing.T) {
		for _, f := range elementsOf(root, "form") {
			if attrOf(f, "id") == "composer" {
				assert.Equal(t, "/c/"+convID+"/messages", attrOf(f, "hx-post"))
				return
			}
		}
		t.Fatal("a conversation of the history can be continued: no composer")
	})
	t.Run("user messages are plain text, never markup", func(t *testing.T) {
		m := messages[0]
		assert.Equal(t, "user", attrOf(m, "data-role"))
		assert.Equal(t, "complete", attrOf(m, "data-status"))
		assert.Equal(t, "What is **2+2**?\n<script>alert(1)</script>", textContent(byTestID(m, "message-body")[0]))
		assert.Empty(t, elementsOf(m, "script"))
		assert.Empty(t, elementsOf(m, "strong"), "asterisks in a user's message stay asterisks")
	})
	t.Run("a reply is rendered Markdown, and offers its text to copy", func(t *testing.T) {
		m := messages[1]
		assert.Equal(t, "assistant", attrOf(m, "data-role"))
		assert.Equal(t, "complete", attrOf(m, "data-status"))
		body := byTestID(m, "message-body")[0]
		assert.Len(t, elementsOf(body, "strong"), 1)
		assert.Equal(t, "It is 4.", strings.TrimSpace(textContent(body)))
		copyButton := byTestID(m, "copy-message")
		require.Len(t, copyButton, 1)
		assert.Equal(t, "It is **4**.", attrOf(copyButton[0], "data-raw"), "the text as it is, Markdown")
	})
	t.Run("a message of the history is not streaming, and has no Regenerate", func(t *testing.T) {
		for _, m := range messages {
			assert.Empty(t, elementsOf(m, "[sse-connect]"))
			assert.Empty(t, byTestID(m, "regenerate"))
			assert.Empty(t, byTestID(m, "retry"))
		}
		assert.NotContains(t, rec.Body.String(), "sse-connect")
	})
	t.Run("the page has no button for earlier messages when it has them all", func(t *testing.T) {
		assert.Empty(t, byTestID(root, "earlier"))
	})
	t.Run("the Historial marks this conversation", func(t *testing.T) {
		assert.Equal(t, "/history?active="+convID, attrOf(byTestID(root, "history")[0], "hx-get"))
	})
}

func TestConversationPage_AnUnfinishedHistoryOffersEarlierMessages(t *testing.T) {
	h := newChatHarness(t)
	t1 := history(said("101", domain.RoleUser, "hola"))
	t1.Older = "101"
	h.history.On("Transcript", mock.Anything, alice.ID, convID, domain.PageRequest{Size: 100}).Return(t1, nil).Once()

	rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

	root := parseHTML(t, rec.Body.String())
	button := byTestID(root, "earlier")
	require.Len(t, button, 1)
	assert.Equal(t, "/c/"+convID+"/earlier?before=101", attrOf(button[0], "hx-get"))
	assert.Equal(t, "#earlier", attrOf(button[0], "hx-target"))
	assert.Equal(t, "outerHTML", attrOf(button[0], "hx-swap"))
}

func TestConversationPage_WhatIsNotTheUsers(t *testing.T) {
	t.Run("a conversation the history does not have, that was not started here, is not found", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, domain.ErrNotFound).Once()
		h.chat.On("Owns", alice.ID, convID).Return(false).Once()

		rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("one started here, which the history does not know yet, is an empty chat", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, domain.ErrNotFound).Once()
		h.chat.On("Owns", alice.ID, convID).Return(true).Once()

		rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Empty(t, byTestID(root, "message"))
		assert.Len(t, byTestID(root, "composer-input"), 1)
	})
	t.Run("without a history, only what was started here is there", func(t *testing.T) {
		h := newHarnessWith(t, Options{}, false)
		h.chat.On("Owns", alice.ID, convID).Return(false).Once()
		assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, "/c/"+convID, nil, nil).Code)

		h.chat.On("Owns", alice.ID, convID).Return(true).Once()
		assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/c/"+convID, nil, nil).Code)
	})
}

func TestConversationPage_AHistoryThatIsDown(t *testing.T) {
	t.Run("says so, with no composer to write into what cannot be seen", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, fmt.Errorf("%w: down", domain.ErrHistoryUnavailable)).Once()
		h.chat.On("Owns", alice.ID, convID).Return(false).Once()

		rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Contains(t, textContent(byTestID(root, "page-note")[0]), "not available")
		assert.Empty(t, byTestID(root, "composer-input"))
	})
	t.Run("a chat started here goes on", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, fmt.Errorf("%w: down", domain.ErrHistoryUnavailable)).Once()
		h.chat.On("Owns", alice.ID, convID).Return(true).Once()

		rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
	t.Run("any other failure is a 500 that says nothing", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, errors.New("secret detail")).Once()

		rec := h.do(http.MethodGet, "/c/"+convID, nil, nil)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret")
	})
}

func TestConversationPage_AnIDThatIsNotOfTheAgentsFormIsRead_NotContinued(t *testing.T) {
	h := newChatHarness(t)
	legacy := "legacy-conversation-7"
	h.history.On("Transcript", mock.Anything, alice.ID, legacy, mock.Anything).Return(domain.Transcript{ConversationID: legacy, Messages: []domain.TranscriptMessage{said("1", domain.RoleUser, "viejo")}}, nil).Once()

	rec := h.do(http.MethodGet, "/c/"+legacy, nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())
	assert.Len(t, byTestID(root, "message"), 1)
	assert.Empty(t, byTestID(root, "composer-input"), "the agent would not take this id")
	assert.Len(t, byTestID(root, "read-only"), 1)
}

func TestEarlierMessages(t *testing.T) {
	t.Run("the page before, to put where the button was, and the button for the one before that", func(t *testing.T) {
		h := newChatHarness(t)
		page := history(said("51", domain.RoleUser, "antes"), said("52", domain.RoleAssistant, "respuesta <b>x</b>"))
		page.Older = "51"
		h.history.On("Transcript", mock.Anything, alice.ID, convID, domain.PageRequest{Before: "101", Size: 100}).Return(page, nil).Once()

		rec := h.do(http.MethodGet, "/c/"+convID+"/earlier?before=101", nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		root := parseHTML(t, rec.Body.String())
		assert.Len(t, byTestID(root, "message"), 2)
		assert.Equal(t, "/c/"+convID+"/earlier?before=51", attrOf(byTestID(root, "earlier")[0], "hx-get"))
		assert.True(t, strings.Index(rec.Body.String(), `data-testid="earlier"`) < strings.Index(rec.Body.String(), `data-testid="message"`), "the button is above the messages")
	})
	t.Run("the first page has no button", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(history(said("1", domain.RoleUser, "primero")), nil).Once()

		rec := h.do(http.MethodGet, "/c/"+convID+"/earlier?before=51", nil, htmx)

		assert.Empty(t, byTestID(parseHTML(t, rec.Body.String()), "earlier"))
	})
	t.Run("a cursor that is not the history's is a 400", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, domain.PageRequest{Before: "nope", Size: 100}).Return(domain.Transcript{}, domain.ErrInvalidPage).Once()

		assert.Equal(t, http.StatusBadRequest, h.do(http.MethodGet, "/c/"+convID+"/earlier?before=nope", nil, htmx).Code)
	})
	t.Run("a conversation that is not the user's is not found", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, domain.ErrNotFound).Once()

		assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, "/c/"+convID+"/earlier?before=1", nil, htmx).Code)
	})
	t.Run("a history that is down swaps nothing", func(t *testing.T) {
		h := newChatHarness(t)
		h.history.On("Transcript", mock.Anything, alice.ID, convID, mock.Anything).Return(domain.Transcript{}, domain.ErrHistoryUnavailable).Once()

		rec := h.do(http.MethodGet, "/c/"+convID+"/earlier?before=1", nil, htmx)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "none", rec.Header().Get("HX-Reswap"))
	})
	t.Run("without a history there is nothing earlier", func(t *testing.T) {
		h := newHarnessWith(t, Options{}, false)

		assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, "/c/"+convID+"/earlier?before=1", nil, htmx).Code)
	})
}

func TestSendMessage(t *testing.T) {
	t.Run("htmx gets the two bubbles, with the reply opening its stream, and the address of the chat", func(t *testing.T) {
		h := newChatHarness(t)
		turn := service.Turn{User: live(domain.RoleUser, domain.StatusComplete, "hola"), Reply: live(domain.RoleAssistant, domain.StatusPending, "")}
		h.chat.On("Send", mock.Anything, alice.ID, convID, "hola").Return(turn, nil).Once()

		rec := h.do(http.MethodPost, "/c/"+convID+"/messages", url.Values{"content": {"hola"}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "/c/"+convID, rec.Header().Get("HX-Replace-Url"), "a chat started at / is at its own address from now on")
		root := parseHTML(t, rec.Body.String())
		messages := byTestID(root, "message")
		require.Len(t, messages, 2)
		assert.Equal(t, "user", attrOf(messages[0], "data-role"))
		assert.Equal(t, "hola", textContent(byTestID(messages[0], "message-body")[0]))
		assert.Equal(t, "assistant", attrOf(messages[1], "data-role"))
		assert.Equal(t, "pending", attrOf(messages[1], "data-status"))
		assert.Contains(t, rec.Body.String(), `sse-connect="/c/`+convID+`/m/`+turn.Reply.ID.String()+`/stream"`)
		assert.Contains(t, rec.Body.String(), `data-stop-url="/c/`+convID+`/m/`+turn.Reply.ID.String()+`/stop"`)
		assert.Contains(t, rec.Body.String(), `data-testid="composer-error"`, "the line of problems is cleared")
	})

	t.Run("what the user writes is text", func(t *testing.T) {
		h := newChatHarness(t)
		text := `<img src=x onerror=alert(1)> **no**`
		turn := service.Turn{User: live(domain.RoleUser, domain.StatusComplete, text), Reply: live(domain.RoleAssistant, domain.StatusPending, "")}
		h.chat.On("Send", mock.Anything, alice.ID, convID, text).Return(turn, nil).Once()

		rec := h.do(http.MethodPost, "/c/"+convID+"/messages", url.Values{"content": {text}}, htmx)

		root := parseHTML(t, rec.Body.String())
		assert.Empty(t, elementsOf(root, "img"))
		assert.Equal(t, text, textContent(byTestID(root, "message-body")[0]))
	})

	t.Run("without htmx there is nothing to stream to: the page is shown, nothing is sent", func(t *testing.T) {
		h := newChatHarness(t) // the chat has no Send expected
		h.chat.On("Owns", alice.ID, convID).Return(true).Once()

		rec := h.do(http.MethodPost, "/c/"+convID+"/messages", url.Values{"content": {"hola"}}, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID, rec.Header().Get("Location"))

		h.chat.On("Owns", alice.ID, convID).Return(false).Once()
		assert.Equal(t, http.StatusNotFound, h.do(http.MethodPost, "/c/"+convID+"/messages", url.Values{"content": {"hola"}}, nil).Code,
			"a conversation that is not theirs is not found, which is what a missing one is")
	})

	problems := []struct {
		name   string
		err    error
		status int
		says   string
	}{
		{"an empty message", domain.ErrEmptyMessage, http.StatusUnprocessableEntity, "Write a message first."},
		{"a message that is too long", domain.TooLong(10), http.StatusUnprocessableEntity, "limit is 10"},
		{"a turn in flight", domain.ErrInvalidState, http.StatusConflict, "Wait for the current reply"},
		{"a history that cannot be asked", fmt.Errorf("%w: down", domain.ErrHistoryUnavailable), http.StatusServiceUnavailable, "Try again"},
		{"a server that is busy", domain.ErrBusy, http.StatusServiceUnavailable, "Try again"},
	}
	for _, tc := range problems {
		t.Run(tc.name+" is said on the composer, which swaps nothing else", func(t *testing.T) {
			h := newChatHarness(t)
			h.chat.On("Send", mock.Anything, alice.ID, convID, "x").Return(service.Turn{}, tc.err).Once()

			rec := h.do(http.MethodPost, "/c/"+convID+"/messages", url.Values{"content": {"x"}}, htmx)

			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, "none", rec.Header().Get("HX-Reswap"))
			assert.Contains(t, rec.Body.String(), tc.says)
			assert.Empty(t, rec.Header().Get("HX-Replace-Url"))
		})
	}

	t.Run("an id that is not a conversation, or a conversation that is not the user's, is not found", func(t *testing.T) {
		for _, failure := range []error{domain.ErrInvalidConversationID, domain.ErrNotFound} {
			h := newChatHarness(t)
			h.chat.On("Send", mock.Anything, alice.ID, "x", "hola").Return(service.Turn{}, failure).Once()

			assert.Equal(t, http.StatusNotFound, h.do(http.MethodPost, "/c/x/messages", url.Values{"content": {"hola"}}, htmx).Code)
		}
	})

	t.Run("any other failure is a 500 that says nothing", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "x").Return(service.Turn{}, errors.New("secret detail")).Once()

		rec := h.do(http.MethodPost, "/c/"+convID+"/messages", url.Values{"content": {"x"}}, htmx)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret")
	})
}

func TestTheOldRoutesAreGone(t *testing.T) {
	h := newChatHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/c"},
		{http.MethodPatch, "/c/" + convID},
		{http.MethodDelete, "/c/" + convID},
		{http.MethodGet, "/c/" + convID + "/rename"},
		{http.MethodGet, "/c/" + convID + "/delete"},
		{http.MethodGet, "/c/" + convID + "/item"},
		{http.MethodPost, "/c/" + convID + "/m/" + uuid.NewV4().String() + "/regenerate"},
		{http.MethodGet, "/c/" + convID + "/m/" + uuid.NewV4().String() + "/raw"},
		{http.MethodGet, "/c/" + convID + "/m/" + uuid.NewV4().String() + "/card"},
	} {
		rec := h.do(tc.method, tc.path, url.Values{}, htmx)

		assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, rec.Code, "%s %s", tc.method, tc.path)
	}
}

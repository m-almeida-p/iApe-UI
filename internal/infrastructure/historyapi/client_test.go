package historyapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/config"
	"iape-ui/internal/infrastructure/historyapi"
)

// The account the fixtures were made for, and the id of the conversation of messages_page.json.
const (
	accountText = "25906ac5-0000-4000-8000-000000000001"
	problemTag  = "tag:yape.com.pe,2026:conversation-history:"
)

var account = uuid.MustParse(accountText)

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(raw)
}

// history serves handler and returns a client for it, with the requests it got.
func history(t *testing.T, handler http.HandlerFunc) (*historyapi.Client, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return historyapi.NewClient(config.History{BaseURL: srv.URL, RequestTimeout: 5 * time.Second}, nil), &seen
}

func serveJSON(status int, link, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if link != "" {
			w.Header().Set("Link", link)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func serveProblem(status int, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"type":%q,"title":"x","status":%d,"detail":"the account `+accountText+` is not here"}`, kind, status)
	}
}

func TestConversations_ReadsTheListOfTheHistory(t *testing.T) {
	c, seen := history(t, serveJSON(200, strings.TrimSpace(fixture(t, "conversations_page.link")), fixture(t, "conversations_page.json")))

	list, err := c.Conversations(t.Context(), account, domain.PageRequest{Size: 3})

	require.NoError(t, err)
	require.Len(t, list.Conversations, 3)
	first := list.Conversations[0]
	assert.Equal(t, "conv_v1_11111111-1111-4111-8111-000000000002", first.ID)
	assert.Equal(t, "quiero yapear", first.FirstMessage)
	assert.False(t, first.FirstMessageTruncated)
	assert.Equal(t, 5, first.MessageCount)
	assert.Equal(t, "1795192236801283~conv_v1_11111111-1111-4111-8111-000000000002", first.Cursor)
	assert.Equal(t, time.UnixMilli(1795192210280), first.FirstAt)
	assert.Equal(t, time.UnixMilli(1795192236801), first.LastAt)
	assert.Equal(t, "1795192107588090~conv_v1_11111111-1111-4111-8111-000000000004", list.Older)
	assert.Empty(t, list.Newer, "the newest page has no newer one")

	require.Len(t, *seen, 1)
	req := (*seen)[0]
	assert.Equal(t, "/v1/conversations", req.URL.Path)
	assert.Equal(t, accountText, req.URL.Query().Get("idYapeAccount"))
	assert.Equal(t, "3", req.URL.Query().Get("page[size]"))
	assert.Equal(t, "application/json", req.Header.Get("Accept"))
	assert.Empty(t, req.Header.Get("Authorization"), "the history has no authentication to send")
}

func TestConversations_SendsTheCursorOfTheDirectionAsked(t *testing.T) {
	tests := []struct {
		name string
		page domain.PageRequest
		key  string
		not  string
	}{
		{"older", domain.PageRequest{Before: "1795192107588090~conv_v1_x"}, "page[before]", "page[after]"},
		{"newer", domain.PageRequest{After: "1795192107588090~conv_v1_x"}, "page[after]", "page[before]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := history(t, serveJSON(200, "", `{"conversations":[]}`))

			_, err := c.Conversations(t.Context(), account, tc.page)

			require.NoError(t, err)
			q := (*seen)[0].URL.Query()
			assert.Equal(t, "1795192107588090~conv_v1_x", q.Get(tc.key))
			assert.False(t, q.Has(tc.not))
			assert.False(t, q.Has("page[size]"), "no size asked: the history's default")
		})
	}
}

func TestConversations_AnEmptyListIsAnEmptyListNotAnError(t *testing.T) {
	for _, body := range []string{`{"conversations":[]}`, `{"conversations":null}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			c, _ := history(t, serveJSON(200, "", body))

			list, err := c.Conversations(t.Context(), account, domain.PageRequest{})

			require.NoError(t, err)
			assert.NotNil(t, list.Conversations)
			assert.Empty(t, list.Conversations)
			assert.Empty(t, list.Older)
			assert.Empty(t, list.Newer)
		})
	}
}

func TestConversations_KeepsWhatTheFirstMessageIs(t *testing.T) {
	body := `{"conversations":[{"_id":"conv_v1_a","cursor":"1~conv_v1_a","messageCount":2,"firstReceived":1.5,"lastReceived":2.25,
		"firstMessage":"¿Cuánto cuesta? 🙂 <b>x</b>","firstMessageTruncated":true}]}`
	c, _ := history(t, serveJSON(200, "", body))

	list, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.NoError(t, err)
	assert.Equal(t, "¿Cuánto cuesta? 🙂 <b>x</b>", list.Conversations[0].FirstMessage, "text as it is: the page escapes it")
	assert.True(t, list.Conversations[0].FirstMessageTruncated)
	assert.Equal(t, time.UnixMilli(1500), list.Conversations[0].FirstAt)
	assert.Equal(t, time.UnixMilli(2250), list.Conversations[0].LastAt)
}

func TestConversations_ARowWithoutAnIDIsNotShown(t *testing.T) { // see also the ids that are not ids, below
	c, _ := history(t, serveJSON(200, "", `{"conversations":[{"cursor":"1~x","messageCount":1}]}`))

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	assert.ErrorIs(t, err, domain.ErrHistoryUnavailable)
}

const transcriptID = "conv_v1_11111111-1111-4111-8111-000000000005"

func TestMessages_ReadsTheTranscript(t *testing.T) {
	c, seen := history(t, serveJSON(200, strings.TrimSpace(fixture(t, "messages_page.link")), fixture(t, "messages_page.json")))

	got, err := c.Messages(t.Context(), account, transcriptID, domain.PageRequest{Size: 3})

	require.NoError(t, err)
	assert.Equal(t, transcriptID, got.ConversationID)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, domain.RoleUser, got.Messages[0].Role)
	assert.Equal(t, "p2p", got.Messages[0].Flow, "a message the user writes carries the flow they were in")
	assert.Equal(t, domain.RoleAssistant, got.Messages[1].Role)
	assert.Equal(t, "9", got.Older, "the cursor of the older page is the id of a message")
	assert.Empty(t, got.Newer)
	for i := 1; i < len(got.Messages); i++ {
		assert.Greater(t, got.Messages[i].Seq, got.Messages[i-1].Seq, "oldest first, as the history gives them")
	}

	req := (*seen)[0]
	assert.Equal(t, "/v1/conversations/"+transcriptID+"/messages", req.URL.Path)
	assert.Equal(t, accountText, req.URL.Query().Get("idYapeAccount"))
}

func TestMessages_MapsTheRolesAndKeepsTheText(t *testing.T) {
	c, _ := history(t, serveJSON(200, "", fixture(t, "messages_full.json")))
	full := fixture(t, "messages_full.json")
	require.Contains(t, full, transcriptID, "the fixture is the one of this conversation")

	got, err := c.Messages(t.Context(), account, transcriptID, domain.PageRequest{})

	require.NoError(t, err)
	require.Len(t, got.Messages, 7)
	assert.Equal(t, domain.RoleUser, got.Messages[0].Role)
	assert.Equal(t, "quiero yapear", got.Messages[0].Text)
	assert.Empty(t, got.Messages[0].Flow, "the message that opens a flow has none")
	assert.Equal(t, domain.RoleUser, got.Messages[4].Role)
	assert.Equal(t, `@(Juan Perez) 20 soles con mensaje "[otp]"`, got.Messages[4].Text)
	assert.False(t, got.Messages[0].At.IsZero())
}

func TestMessages_ReadsTheTruncatedMark(t *testing.T) {
	body := `{"conversation":{"_id":"conv_v1_a"},"messages":[{"_id":"1","type":"text","role":"appMaker","text":"…","received":1.5,
		"metadata":{"sequence":1,"truncated":true}}]}`
	c, _ := history(t, serveJSON(200, "", body))

	got, err := c.Messages(t.Context(), account, "conv_v1_a", domain.PageRequest{})

	require.NoError(t, err)
	assert.True(t, got.Messages[0].Truncated)
}

func TestMessages_AMessageThatIsNotTextIsLeftOut(t *testing.T) {
	body := `{"conversation":{"_id":"conv_v1_a"},"messages":[
		{"_id":"1","type":"image","role":"appMaker","text":"","received":1.5,"metadata":{"sequence":1}},
		{"_id":"2","type":"text","role":"appUser","text":"hola","received":2.5,"metadata":{"sequence":2}}]}`
	c, _ := history(t, serveJSON(200, "", body))

	got, err := c.Messages(t.Context(), account, "conv_v1_a", domain.PageRequest{})

	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hola", got.Messages[0].Text)
}

func TestMessages_AnUnknownRoleIsAnAnswerThatBreaksTheContract(t *testing.T) {
	body := `{"conversation":{"_id":"conv_v1_a"},"messages":[{"_id":"1","type":"text","role":"robot","text":"x","received":1.5,"metadata":{"sequence":1}}]}`
	c, _ := history(t, serveJSON(200, "", body))

	_, err := c.Messages(t.Context(), account, "conv_v1_a", domain.PageRequest{})

	assert.ErrorIs(t, err, domain.ErrHistoryUnavailable)
}

func TestMessages_AnAnswerAboutAnotherConversationIsNotShown(t *testing.T) {
	c, _ := history(t, serveJSON(200, "", `{"conversation":{"_id":"conv_v1_other"},"messages":[]}`))

	_, err := c.Messages(t.Context(), account, "conv_v1_a", domain.PageRequest{})

	assert.ErrorIs(t, err, domain.ErrHistoryUnavailable)
}

func TestMessages_TheIDIsAPathSegmentAndNothingMore(t *testing.T) {
	c, seen := history(t, serveJSON(200, "", `{"conversation":{"_id":"a.b:c_d-e"},"messages":[]}`))

	_, err := c.Messages(t.Context(), account, "a.b:c_d-e", domain.PageRequest{})

	require.NoError(t, err)
	assert.Equal(t, "/v1/conversations/a.b:c_d-e/messages", (*seen)[0].URL.EscapedPath())
}

func TestMessages_AnIDThatTheHistoryCannotHaveIsNotLookedFor(t *testing.T) {
	tests := map[string]string{
		"empty":         "",
		"129 chars":     strings.Repeat("a", 129),
		"a slash":       "conv/../x",
		"a space":       "conv x",
		"a query":       "conv?x=1",
		"a newline":     "conv\nx",
		"not ASCII":     "convñ",
		"a percent":     "conv%2e",
		"an upper path": "../../health",
		"a dot":         ".",
		"two dots":      "..",
		"only dots":     "...",
	}
	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			c, seen := history(t, serveJSON(200, "", `{}`))

			_, err := c.Messages(t.Context(), account, id, domain.PageRequest{})

			assert.ErrorIs(t, err, domain.ErrNotFound)
			assert.Empty(t, *seen, "nothing was sent")
		})
	}
}

func TestMessages_AnIDOf128CharsIsLookedFor(t *testing.T) {
	id := strings.Repeat("a", 128)
	c, seen := history(t, serveJSON(200, "", `{"conversation":{"_id":"`+id+`"},"messages":[]}`))

	_, err := c.Messages(t.Context(), account, id, domain.PageRequest{})

	require.NoError(t, err)
	assert.Len(t, *seen, 1)
}

func TestARequestThatTheHistoryWouldRefuseIsNotSent(t *testing.T) {
	bad := []domain.PageRequest{{Before: "1", After: "2"}, {Size: 101}, {Size: -1}, {Before: "a b"}}
	for _, page := range bad {
		t.Run(fmt.Sprintf("%+v", page), func(t *testing.T) {
			c, seen := history(t, serveJSON(200, "", `{}`))

			_, errList := c.Conversations(t.Context(), account, page)
			_, errMessages := c.Messages(t.Context(), account, "conv_v1_a", page)

			assert.ErrorIs(t, errList, domain.ErrInvalidPage)
			assert.ErrorIs(t, errMessages, domain.ErrInvalidPage)
			assert.Empty(t, *seen)
		})
	}
}

func TestARequestWithoutAnAccountIsNotSent(t *testing.T) {
	c, seen := history(t, serveJSON(200, "", `{}`))

	_, errList := c.Conversations(t.Context(), uuid.Nil(), domain.PageRequest{})
	_, errMessages := c.Messages(t.Context(), uuid.Nil(), "conv_v1_a", domain.PageRequest{})

	require.Error(t, errList)
	require.Error(t, errMessages)
	assert.Empty(t, *seen)
}

func TestWhatTheHistoryRefusesWithBecomesAnErrorOfTheDomain(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		is      error
		isNot   error
	}{
		{"the conversation is not there", serveProblem(404, problemTag+"conversation-not-found"), domain.ErrNotFound, domain.ErrHistoryUnavailable},
		{"the cursor is not one of its own", serveProblem(400, problemTag+"invalid-cursor"), domain.ErrInvalidPage, domain.ErrHistoryUnavailable},
		{"the request is malformed", serveProblem(400, problemTag+"invalid-request"), domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"a 404 of a wrong address is not a missing conversation", serveProblem(404, "about:blank"), domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"a 404 with no problem", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }, domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"an unknown problem type", serveProblem(404, "https://example.com/conversation-not-found"), domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"an error of the history", serveProblem(500, "about:blank"), domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"the history is down", serveProblem(503, "about:blank"), domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"a gateway in between", serveProblem(502, "about:blank"), domain.ErrHistoryUnavailable, domain.ErrNotFound},
		{"a refusal that is not JSON", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503); _, _ = w.Write([]byte("<html>")) }, domain.ErrHistoryUnavailable, domain.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := history(t, tc.handler)

			_, errList := c.Conversations(t.Context(), account, domain.PageRequest{})
			_, errMessages := c.Messages(t.Context(), account, "conv_v1_a", domain.PageRequest{})

			for _, err := range []error{errList, errMessages} {
				require.ErrorIs(t, err, tc.is)
				assert.NotErrorIs(t, err, tc.isNot)
			}
		})
	}
}

func TestAnErrorHoldsNeitherTheAccountNorWhatTheHistorySaid(t *testing.T) {
	c, _ := history(t, serveProblem(503, "about:blank"))

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), accountText, "the detail of the history names the account")
	assert.NotContains(t, err.Error(), "is not here")
	httpErr, ok := errors.AsType[*historyapi.HTTPError](err)
	require.True(t, ok)
	assert.Equal(t, 503, httpErr.Status)
	assert.Contains(t, err.Error(), "HTTP 503")
}

func TestAHistoryThatIsNotThereIsUnavailableAndTheErrorHoldsNoAccount(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := historyapi.NewClient(config.History{BaseURL: url, RequestTimeout: time.Second}, nil)

	_, errList := c.Conversations(t.Context(), account, domain.PageRequest{})
	_, errMessages := c.Messages(t.Context(), account, "conv_v1_a", domain.PageRequest{})

	for _, err := range []error{errList, errMessages} {
		require.ErrorIs(t, err, domain.ErrHistoryUnavailable)
		assert.NotContains(t, err.Error(), accountText, "net/http names the address, and its query has the account")
		assert.NotContains(t, err.Error(), "idYapeAccount")
	}
}

func TestAnAnswerThatIsNotWhatTheHistoryPromisesIsUnavailable(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"HTML": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<p>hi</p>"))
		},
		"no content type": func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(`{}`))
		},
		"JSON that is cut":     serveJSON(200, "", `{"conversations":[{"_id":"a"`),
		"JSON of another kind": serveJSON(200, "", `[1,2,3]`),
		"a number for a text":  serveJSON(200, "", `{"conversations":[{"_id":"a","firstMessage":5}]}`),
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := history(t, h)

			_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

			assert.ErrorIs(t, err, domain.ErrHistoryUnavailable)
		})
	}
}

func TestAResponseOfFourMegabytesIsRefused(t *testing.T) {
	filler := strings.Repeat("x", 4<<20)
	c, _ := history(t, serveJSON(200, "", `{"conversations":[{"_id":"a","firstMessage":"`+filler+`"}]}`))

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.ErrorIs(t, err, domain.ErrHistoryUnavailable)
	assert.Contains(t, err.Error(), "larger than")
}

func TestAResponseJustUnderTheLimitIsRead(t *testing.T) {
	const head, tail = `{"conversations":[{"_id":"a","firstMessage":"`, `"}]}`
	filler := strings.Repeat("x", 4<<20-len(head)-len(tail))
	c, _ := history(t, serveJSON(200, "", head+filler+tail))

	list, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.NoError(t, err)
	assert.Len(t, list.Conversations[0].FirstMessage, len(filler))
}

func TestAHistoryThatIsTooSlowTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	c := historyapi.NewClient(config.History{BaseURL: srv.URL, RequestTimeout: 50 * time.Millisecond}, nil)
	start := time.Now()

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.ErrorIs(t, err, domain.ErrHistoryUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "timed out after 50ms")
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestAHistoryThatStopsInTheMiddleOfTheBodyTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"conv`))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c := historyapi.NewClient(config.History{BaseURL: srv.URL, RequestTimeout: 100 * time.Millisecond}, nil)

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.ErrorIs(t, err, domain.ErrHistoryUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestACallerThatLeavesIsNotAFailureOfTheHistory(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	c := historyapi.NewClient(config.History{BaseURL: srv.URL, RequestTimeout: time.Minute}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := c.Conversations(ctx, account, domain.PageRequest{})

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, domain.ErrHistoryUnavailable)
}

func TestNoTimeoutSettingMeansNoLimit(t *testing.T) {
	srv := httptest.NewServer(serveJSON(200, "", `{"conversations":[]}`))
	t.Cleanup(srv.Close)
	c := historyapi.NewClient(config.History{BaseURL: srv.URL}, nil)

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	assert.NoError(t, err)
}

func TestARedirectIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	c, _ := history(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.ErrorIs(t, err, domain.ErrHistoryUnavailable)
	assert.Zero(t, elsewhere.Load(), "the account went nowhere else")
}

func TestTheAddressMayEndInASlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		serveJSON(200, "", `{"conversations":[]}`)(w, r)
	}))
	t.Cleanup(srv.Close)
	c := historyapi.NewClient(config.History{BaseURL: srv.URL + "/", RequestTimeout: time.Second}, nil)

	_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

	require.NoError(t, err)
	assert.Equal(t, "/v1/conversations", path)
}

func TestLink_TakesTheCursorsOfBothDirections(t *testing.T) {
	prev := `</v1/conversations?idYapeAccount=a&page[before]=1795~conv_v1_x&page[size]=2>; rel="prev"`
	next := `</v1/conversations?idYapeAccount=a&page[after]=1799~conv_v1_y&page[size]=2>; rel="next"`
	tests := []struct {
		name         string
		values       []string
		older, newer string
	}{
		{"two header lines", []string{prev, next}, "1795~conv_v1_x", "1799~conv_v1_y"},
		{"one line with a comma", []string{prev + ", " + next}, "1795~conv_v1_x", "1799~conv_v1_y"},
		{"one line, next first", []string{next + "," + prev}, "1795~conv_v1_x", "1799~conv_v1_y"},
		{"only prev", []string{prev}, "1795~conv_v1_x", ""},
		{"only next", []string{next}, "", "1799~conv_v1_y"},
		{"no header", nil, "", ""},
		{"rel without quotes", []string{`</v1/c?page[before]=7>; rel=prev`}, "7", ""},
		{"rel in capitals", []string{`</v1/c?page[before]=7>; REL="Prev"`}, "7", ""},
		{"another parameter before rel", []string{`</v1/c?page[before]=7>; title="x"; rel="prev"`}, "7", ""},
		{"several rels in one", []string{`</v1/c?page[before]=7>; rel="prev archive"`}, "7", ""},
		{"an absolute address of another host: only the cursor is used", []string{`<https://evil.example/x?page[before]=7>; rel="prev"`}, "7", ""},
		{"a rel that is not ours", []string{`</v1/c?page[before]=7>; rel="first"`}, "", ""},
		{"no rel", []string{`</v1/c?page[before]=7>`}, "", ""},
		{"a link without a cursor", []string{`</v1/c?idYapeAccount=a>; rel="prev"`}, "", ""},
		{"a cursor that no request may carry", []string{`</v1/c?page[before]=a%20b>; rel="prev"`}, "", ""},
		{"the wrong parameter for the direction", []string{`</v1/c?page[after]=7>; rel="prev"`}, "", ""},
		{"garbage", []string{`<<<>>>; rel=`, `no angle brackets; rel="prev"`, `<unterminated; rel="prev"`}, "", ""},
		{"an address that does not parse", []string{"<%zz>; rel=\"prev\""}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := history(t, func(w http.ResponseWriter, _ *http.Request) {
				for _, v := range tc.values {
					w.Header().Add("Link", v)
				}
				serveJSON(200, "", `{"conversations":[]}`)(w, nil)
			})

			list, err := c.Conversations(t.Context(), account, domain.PageRequest{})

			require.NoError(t, err)
			assert.Equal(t, tc.older, list.Older)
			assert.Equal(t, tc.newer, list.Newer)
		})
	}
}

func TestHTTPError_SaysWhatItKnows(t *testing.T) {
	assert.Equal(t, "history returned HTTP 503", (&historyapi.HTTPError{Status: 503}).Error())
	assert.Equal(t, "history returned HTTP 400 (invalid-request)", (&historyapi.HTTPError{Status: 400, Problem: "invalid-request"}).Error())
}

func TestConversations_AnIDThatIsNotOneIsAnAnswerThatBreaksTheContract(t *testing.T) {
	for name, id := range map[string]string{"a path": "../x", "a dot": ".", "a space": "a b", "a query": "a?b", "too long": strings.Repeat("a", 129)} {
		t.Run(name, func(t *testing.T) {
			c, _ := history(t, serveJSON(200, "", `{"conversations":[{"_id":"`+id+`","cursor":"1~x","messageCount":1}]}`))

			_, err := c.Conversations(t.Context(), account, domain.PageRequest{})

			assert.ErrorIs(t, err, domain.ErrHistoryUnavailable)
		})
	}
}

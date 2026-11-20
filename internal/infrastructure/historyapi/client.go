// Package historyapi reads the conversation history over HTTP: the API of ms-ml-conv-history-bs. It implements
// resource.HistoryGateway.
//
// The history has no authentication yet (a proof of concept, not to be deployed), so a request carries no token. Who a request is
// for is the account in its query, and the history answers with what that account owns.
package historyapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/infrastructure/config"
)

var _ resource.HistoryGateway = (*Client)(nil)

const (
	// maxBody is the most of a response that is read: a page of 100 messages, each cut by the history at its limit, is far below it.
	maxBody = 4 << 20
	// maxProblemBody is how much of a refusal is read to find its type.
	maxProblemBody = 16 << 10
	// maxIDChars is the longest conversation id the history takes.
	maxIDChars  = 128
	dialTimeout = 5 * time.Second

	problemInvalidCursor = "invalid-cursor"
	problemNotFound      = "conversation-not-found"
)

// HTTPError is a refusal by the history. It holds the status and the kind of problem and nothing the history said, which can name
// what the request asked for.
type HTTPError struct {
	Status int
	// Problem is the last part of the problem type, such as conversation-not-found. Empty when the response was none.
	Problem string
}

func (e *HTTPError) Error() string {
	if e.Problem == "" {
		return fmt.Sprintf("history returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("history returned HTTP %d (%s)", e.Status, e.Problem)
}

// Client reads the history. It implements resource.HistoryGateway.
type Client struct {
	base    string
	timeout time.Duration
	http    *http.Client
}

// NewClient makes a client for the history at cfg.BaseURL. A nil httpClient gets a transport with a dial timeout, which follows
// no redirect: the history is at the address of the settings and nowhere else.
func NewClient(cfg config.History, httpClient *http.Client) *Client {
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
		transport.MaxIdleConnsPerHost = 32
		httpClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Client{base: strings.TrimRight(cfg.BaseURL, "/"), timeout: cfg.RequestTimeout, http: httpClient}
}

// Conversations implements resource.HistoryGateway.
func (c *Client) Conversations(ctx context.Context, account uuid.UUID, page domain.PageRequest) (domain.ConversationList, error) {
	query, err := pageQuery(account, page)
	if err != nil {
		return domain.ConversationList{}, err
	}
	var body struct {
		Conversations []conversationJSON `json:"conversations"`
	}
	header, err := c.get(ctx, "/v1/conversations", query, &body)
	if err != nil {
		return domain.ConversationList{}, err
	}
	list := domain.ConversationList{Conversations: make([]domain.ConversationSummary, 0, len(body.Conversations))}
	for _, cj := range body.Conversations {
		if cj.ID == "" {
			return domain.ConversationList{}, fmt.Errorf("%w: a conversation without an id", domain.ErrHistoryUnavailable)
		}
		list.Conversations = append(list.Conversations, domain.ConversationSummary{
			ID:                    cj.ID,
			Cursor:                cj.Cursor,
			MessageCount:          cj.MessageCount,
			FirstAt:               unixTime(cj.FirstReceived),
			LastAt:                unixTime(cj.LastReceived),
			FirstMessage:          cj.FirstMessage,
			FirstMessageTruncated: cj.FirstMessageTruncated,
		})
	}
	list.Older, list.Newer = cursors(header)
	return list, nil
}

// Messages implements resource.HistoryGateway.
func (c *Client) Messages(ctx context.Context, account uuid.UUID, conversationID string, page domain.PageRequest) (domain.Transcript, error) {
	if !validID(conversationID) { // not an id the history has: it belongs to no one
		return domain.Transcript{}, domain.ErrNotFound
	}
	query, err := pageQuery(account, page)
	if err != nil {
		return domain.Transcript{}, err
	}
	var body struct {
		Conversation struct {
			ID string `json:"_id"`
		} `json:"conversation"`
		Messages []messageJSON `json:"messages"`
	}
	header, err := c.get(ctx, "/v1/conversations/"+url.PathEscape(conversationID)+"/messages", query, &body)
	if err != nil {
		return domain.Transcript{}, err
	}
	if body.Conversation.ID != conversationID { // an answer about another conversation is not shown as this one
		return domain.Transcript{}, fmt.Errorf("%w: the answer is about another conversation", domain.ErrHistoryUnavailable)
	}
	transcript := domain.Transcript{ConversationID: conversationID, Messages: make([]domain.TranscriptMessage, 0, len(body.Messages))}
	for _, mj := range body.Messages {
		if mj.Type != "text" { // the history says text is all there is; a message of another kind has no way to be shown
			continue
		}
		role, ok := roles[mj.Role]
		if !ok {
			return domain.Transcript{}, fmt.Errorf("%w: a message of an unknown role", domain.ErrHistoryUnavailable)
		}
		transcript.Messages = append(transcript.Messages, domain.TranscriptMessage{
			ID:        mj.ID,
			Seq:       mj.Metadata.Sequence,
			Role:      role,
			Text:      mj.Text,
			Truncated: mj.Metadata.Truncated,
			At:        unixTime(mj.Received),
			Flow:      mj.Metadata.Flow,
		})
	}
	transcript.Older, transcript.Newer = cursors(header)
	return transcript, nil
}

// roles maps who wrote a message in the history to who it is in the page: the customer, or the assistant with its guardrail replies.
var roles = map[string]domain.Role{"appUser": domain.RoleUser, "appMaker": domain.RoleAssistant}

type conversationJSON struct {
	ID                    string  `json:"_id"`
	Cursor                string  `json:"cursor"`
	MessageCount          int     `json:"messageCount"`
	FirstReceived         float64 `json:"firstReceived"`
	LastReceived          float64 `json:"lastReceived"`
	FirstMessage          string  `json:"firstMessage"`
	FirstMessageTruncated bool    `json:"firstMessageTruncated"`
}

type messageJSON struct {
	ID       string  `json:"_id"`
	Type     string  `json:"type"`
	Role     string  `json:"role"`
	Text     string  `json:"text"`
	Received float64 `json:"received"`
	Metadata struct {
		Sequence  int    `json:"sequence"`
		Truncated bool   `json:"truncated"`
		Flow      string `json:"flow"`
	} `json:"metadata"`
}

// unixTime is a time the history gives, in seconds with milliseconds as decimals.
func unixTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(math.Round(seconds * 1000)))
}

// validID reports whether s can be the id of a conversation: what the history accepts, letters, digits and . _ : -, up to 128.
func validID(s string) bool {
	if s == "" || len(s) > maxIDChars {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:-", c) >= 0) {
			return false
		}
	}
	return true
}

// pageQuery is the query of a request for the account's page. A request that the history would refuse is not made.
func pageQuery(account uuid.UUID, page domain.PageRequest) (url.Values, error) {
	if account == uuid.Nil() {
		return nil, errors.New("history: a request needs an account")
	}
	if err := page.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{"idYapeAccount": {account.String()}}
	if page.Before != "" {
		q.Set("page[before]", page.Before)
	}
	if page.After != "" {
		q.Set("page[after]", page.After)
	}
	if page.Size > 0 {
		q.Set("page[size]", strconv.Itoa(page.Size))
	}
	return q, nil
}

// get makes the request and decodes the answer into out. It returns the headers of the response.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) (http.Header, error) {
	runCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, c.base+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrHistoryUnavailable, urlFree(err))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.fail(ctx, runCtx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" {
		return nil, fmt.Errorf("%w: unexpected content type %q", domain.ErrHistoryUnavailable, resp.Header.Get("Content-Type"))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, c.fail(ctx, runCtx, err)
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("%w: the response is larger than %d bytes", domain.ErrHistoryUnavailable, maxBody)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("%w: the response is not what the history promises", domain.ErrHistoryUnavailable)
	}
	return resp.Header, nil
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// fail tells what ended a request: the caller leaving, the time limit, or the failure itself.
func (c *Client) fail(caller, run context.Context, err error) error {
	switch {
	case caller.Err() != nil:
		return caller.Err()
	case run.Err() != nil:
		return fmt.Errorf("%w: the request timed out after %s: %w", domain.ErrHistoryUnavailable, c.timeout, context.DeadlineExceeded)
	}
	return fmt.Errorf("%w: %w", domain.ErrHistoryUnavailable, urlFree(err))
}

// urlFree is err without the address that net/http names in it: the query of a request holds the account.
func urlFree(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// statusError is the error for a response that is not a success. Only a problem that says that the conversation is not found is
// ErrNotFound: a 404 of any other kind is a wrong address, and the history is then not working for this page.
func statusError(resp *http.Response) error {
	problem := problemKind(resp)
	switch {
	case resp.StatusCode == http.StatusNotFound && problem == problemNotFound:
		return domain.ErrNotFound
	case resp.StatusCode == http.StatusBadRequest && problem == problemInvalidCursor:
		return domain.ErrInvalidPage
	}
	return fmt.Errorf("%w: %w", domain.ErrHistoryUnavailable, &HTTPError{Status: resp.StatusCode, Problem: problem})
}

// problemKind is the last part of the type of a problem+json response, or "" when the response is no problem or its type is
// not one of the history's tags.
func problemKind(resp *http.Response) string {
	var p struct {
		Type string `json:"type"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxProblemBody))
	if json.Unmarshal(data, &p) != nil {
		return ""
	}
	const tag = "tag:yape.com.pe,2026:conversation-history:"
	kind, ok := strings.CutPrefix(p.Type, tag)
	if !ok || kind == "" || strings.ContainsAny(kind, " :/") {
		return ""
	}
	return kind
}

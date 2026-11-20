package stack

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// NewChat opens the empty-state page and returns the id the chat will have: the one the composer posts to.
func (c *Client) NewChat() (conv string) {
	c.t.Helper()
	res := c.Get("/")
	require.Equal(c.t, http.StatusOK, res.Status, res.Body)
	form := Find(Doc(c.t, res.Body), func(n *html.Node) bool { return n.Type == html.ElementNode && Attr(n, "id") == "composer" })
	require.NotNil(c.t, form, "the page has a composer")
	action := Attr(form, "hx-post")
	require.True(c.t, strings.HasPrefix(action, "/c/") && strings.HasSuffix(action, "/messages"), "the composer posts to %q", action)
	return strings.TrimSuffix(strings.TrimPrefix(action, "/c/"), "/messages")
}

// Start opens a chat with its first message, as the empty-state page does, and returns the ids of the conversation and of the
// reply being written.
func (c *Client) Start(content string) (conv, reply string) {
	c.t.Helper()
	conv = c.NewChat()
	return conv, c.Send(conv, content)
}

// Send adds a message to a conversation the way the composer does (htmx), and
// returns the id of the reply being written.
func (c *Client) Send(conv, content string) (reply string) {
	c.t.Helper()
	res := c.Post("/c/"+conv+"/messages", url.Values{"content": {content}}, "HX-Request", "true")
	require.Equal(c.t, http.StatusOK, res.Status, res.Body)
	messages := MessagesIn(c.t, res.Body)
	require.Len(c.t, messages, 2, "the answer holds the new message and the reply: %s", res.Body)
	require.Equal(c.t, "assistant", messages[1].Role)
	return messages[1].ID
}

// Stop presses Stop on a reply.
func (c *Client) Stop(conv, reply string) *Response {
	c.t.Helper()
	return c.Post("/c/"+conv+"/m/"+reply+"/stop", nil, "HX-Request", "true")
}

// RawOf is the text of a finished reply exactly as the page's Copy button holds it (Markdown), or "" when it has none.
func RawOf(m Message) string {
	button := Find(m.Node, TestID("copy-message"))
	if button == nil {
		return ""
	}
	return Attr(button, "data-raw")
}

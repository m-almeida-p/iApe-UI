// The Web UI in front of the real agent and the real history, from a first message to the same conversation read back and continued,
// and a payment of the test bank from a card. Skipped unless the environment or .env says AGENT_LIVE=1 and HISTORY_LIVE=1:
//
//	AGENT_LIVE=1 HISTORY_LIVE=1 HISTORY_BASE_URL=http://localhost:3100 go test ./it/live/ -run TestLiveFlow -v
//
// The agent is the BFF in .env (AGENT_BASE_URL, YAPE_TOKEN_BIN), the account YAPE_ACCOUNT_ID, and the history HISTORY_BASE_URL. The
// pipeline from the agent to the history (Debezium, Kafka, the consumer) has to be running: the test waits for it.
package live_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/platform/envcfg"
	"iape-ui/it/internal/stack"
)

func TestLiveFlow(t *testing.T) {
	lookup, err := envcfg.EnvLookup("../../.env")
	require.NoError(t, err)
	if v, _ := lookup("AGENT_LIVE"); v != "1" {
		t.Skip("set AGENT_LIVE=1 and HISTORY_LIVE=1 to run the flow against the real agent and history")
	}
	if v, _ := lookup("HISTORY_LIVE"); v != "1" {
		t.Skip("set HISTORY_LIVE=1 too")
	}
	agentURL, _ := lookup("AGENT_BASE_URL")
	account, _ := lookup("YAPE_ACCOUNT_ID")
	historyURL, _ := lookup("HISTORY_BASE_URL")
	bin, _ := lookup("YAPE_TOKEN_BIN")
	require.NotEmpty(t, agentURL)
	require.NotEmpty(t, account)
	require.NotEmpty(t, historyURL)
	opts := []stack.Option{
		stack.WithExternalAgent(agentURL), stack.WithSetting("YAPE_ACCOUNT_ID", account), stack.WithSetting("HISTORY_BASE_URL", historyURL),
		stack.WithSetting("AGENT_REQUEST_TIMEOUT", "300s"),
	}
	if bin != "" {
		opts = append(opts, stack.WithSetting("YAPE_TOKEN_BIN", bin))
	}
	s := stack.Start(t, opts...)
	c := s.SignedIn()
	patient := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		t.Cleanup(cancel)
		return ctx
	}
	finalOf := func(events []stack.Event) stack.Message {
		deltas := stack.Named(events, "delta")
		require.NotEmpty(t, deltas)
		return stack.MessagesIn(t, deltas[len(deltas)-1].Data)[0]
	}

	// seen waits for the history to show the conversation, which takes about a second.
	seen := func(conv, what string) string {
		var body string
		require.Eventually(t, func() bool {
			body = c.Get("/history", "HX-Request", "true").Body
			return strings.Contains(body, conv) && strings.Contains(body, what)
		}, 30*time.Second, 500*time.Millisecond, "the history lists the conversation with %q", what)
		return body
	}

	conv := c.NewChat()
	t.Logf("the chat has the id %s", conv)
	first := c.Send(conv, "quiero yapear")
	events := c.OpenStream(patient(), conv, first).Drain()
	reply := finalOf(events)
	require.Equal(t, "complete", reply.Status, "the agent answered: %q", reply.Text)
	t.Logf("the agent said: %.100q", reply.Text)

	t.Run("the conversation reaches the history under the id the UI made, with its first message", func(t *testing.T) {
		list := seen(conv, "quiero yapear")
		assert.Contains(t, list, `data-id="`+conv+`"`)
		assert.NotContains(t, list, account, "the page never shows the account")
	})

	t.Run("it opens as a chat, with what was said", func(t *testing.T) {
		var messages []stack.Message
		require.Eventually(t, func() bool {
			messages = stack.MessagesIn(t, c.Get("/c/"+conv).Body)
			return len(messages) >= 2
		}, 30*time.Second, 500*time.Millisecond)
		assert.Equal(t, "user", messages[0].Role)
		assert.Equal(t, "quiero yapear", strings.TrimSpace(messages[0].Text))
		assert.Equal(t, "assistant", messages[1].Role)
		assert.NotEmpty(t, strings.TrimSpace(messages[1].Text))
	})

	t.Run("it is continued, and the history gets the new turn under the same conversation", func(t *testing.T) {
		before := len(stack.MessagesIn(t, c.Get("/c/"+conv).Body))
		next := c.Send(conv, "y si fuera a otra persona?")
		events := c.OpenStream(patient(), conv, next).Drain()
		require.Equal(t, "complete", finalOf(events).Status)
		require.Eventually(t, func() bool {
			return len(stack.MessagesIn(t, c.Get("/c/"+conv).Body)) >= before+2
		}, 30*time.Second, 500*time.Millisecond, "the history holds the second turn too")
		seen(conv, "quiero yapear")
	})

	t.Run("another user sees nothing of it", func(t *testing.T) {
		s.AddUser("otro@example.com", "another-long-enough-password")
		other := s.SignedInAs("otro@example.com", "another-long-enough-password")

		assert.NotContains(t, other.Get("/history", "HX-Request", "true").Body, conv)
		assert.Equal(t, 404, other.Get("/c/"+conv).Status, "it is not theirs, and it is answered as a conversation that does not exist")
		assert.Equal(t, 404, other.Post("/c/"+conv+"/messages", url.Values{"content": {"hola"}}, "HX-Request", "true").Status,
			"nor can they write into it")
	})

	t.Run("a payment of the test bank from a card, and a second click on it", func(t *testing.T) {
		pay := c.NewChat()
		turn := c.Send(pay, "quiero yapear")
		c.OpenStream(patient(), pay, turn).Drain()
		// The picker tells the agent's service whom the user chose, as the composer's @ does.
		picked := c.Post("/contacts/confirm", url.Values{"q": {"Juan Perez"}, "choice": {"0"}}, "HX-Request", "true")
		require.Equal(t, 200, picked.Status, picked.Body)
		turn = c.Send(pay, "@(Juan Perez) 20 soles")
		final := finalOf(c.OpenStream(patient(), pay, turn).Drain())
		t.Logf("the agent said: %.120q", final.Text)
		button := stack.Find(final.Node, stack.TestID("card-pay"))
		if button == nil {
			t.Skip("the agent did not put a card to pay in the reply (it may ask for more first): the page shows what it said above")
		}
		approve := stack.Attr(button, "hx-post")
		require.NotEmpty(t, approve)

		first := c.Post(approve, nil, "HX-Request", "true")
		second := c.Post(approve, nil, "HX-Request", "true")

		require.Equal(t, 200, first.Status, first.Body)
		card := stack.Find(stack.Doc(t, first.Body), stack.TestID("card"))
		require.NotNil(t, card)
		t.Logf("the card after the first click: state=%s result=%.100q", stack.Attr(card, "data-state"), stack.Text(stack.Find(card, stack.TestID("card-result"))))
		assert.Contains(t, []string{"paid", "otp_required"}, stack.Attr(card, "data-state"))
		again := stack.Find(stack.Doc(t, second.Body), stack.TestID("card"))
		if second.Status == 200 && again != nil {
			t.Logf("the card after the second click: state=%s", stack.Attr(again, "data-state"))
			assert.Equal(t, stack.Attr(card, "data-state"), stack.Attr(again, "data-state"), "the second click shows what the first did, and pays nothing")
		}
	})
}

// The edges of the flow against the real services: what a second tab sees, a reload in the middle of a reply, and the second or so in which
// the history does not have the turn yet.
func TestLiveEdges(t *testing.T) {
	lookup, err := envcfg.EnvLookup("../../.env")
	require.NoError(t, err)
	if v, _ := lookup("AGENT_LIVE"); v != "1" {
		t.Skip("set AGENT_LIVE=1 and HISTORY_LIVE=1 to run the flow against the real agent and history")
	}
	if v, _ := lookup("HISTORY_LIVE"); v != "1" {
		t.Skip("set HISTORY_LIVE=1 too")
	}
	agentURL, _ := lookup("AGENT_BASE_URL")
	account, _ := lookup("YAPE_ACCOUNT_ID")
	historyURL, _ := lookup("HISTORY_BASE_URL")
	bin, _ := lookup("YAPE_TOKEN_BIN")
	opts := []stack.Option{
		stack.WithExternalAgent(agentURL), stack.WithSetting("YAPE_ACCOUNT_ID", account), stack.WithSetting("HISTORY_BASE_URL", historyURL),
		stack.WithSetting("AGENT_REQUEST_TIMEOUT", "300s"),
	}
	if bin != "" {
		opts = append(opts, stack.WithSetting("YAPE_TOKEN_BIN", bin))
	}
	s := stack.Start(t, opts...)
	patient := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("a reload in the middle of a reply shows the page, and writing waits for the reply", func(t *testing.T) {
		c := s.SignedIn()
		conv := c.NewChat()
		reply := c.Send(conv, "quiero yapear")

		page := c.Get("/c/" + conv) // the reply is still being written: it is live here, and not in the history

		assert.Equal(t, 200, page.Status, "the page of a conversation the history does not have yet, started here")
		assert.Contains(t, page.Body, "composer-input")
		busy := c.Post("/c/"+conv+"/messages", url.Values{"content": {"otra cosa"}}, "HX-Request", "true")
		assert.Equal(t, 409, busy.Status, "no second turn while the first is written")
		assert.Contains(t, busy.Body, "Wait for the current reply")
		events := c.OpenStream(patient(), conv, reply).Drain() // the stream is still there to be opened after the reload
		require.NotEmpty(t, events)
		next := c.Send(conv, "y si fuera a otra persona?")
		assert.NotEmpty(t, next, "and once it ends, writing works")
		c.OpenStream(patient(), conv, next).Drain()
	})

	t.Run("two tabs of one user writing at once: one turn is accepted, the other is told to wait", func(t *testing.T) {
		tab1, tab2 := s.SignedIn(), s.SignedIn()
		conv := tab1.NewChat()
		results := make(chan int, 2)
		var wg sync.WaitGroup
		for i, tab := range []*stack.Client{tab1, tab2} {
			wg.Go(func() {
				results <- tab.Post("/c/"+conv+"/messages", url.Values{"content": {fmt.Sprintf("mensaje %d", i)}}, "HX-Request", "true").Status
			})
		}
		wg.Wait()
		close(results)

		got := map[int]int{}
		for status := range results {
			got[status]++
		}
		assert.Equal(t, map[int]int{200: 1, 409: 1}, got)
		// the one that was accepted has a reply to stream: finish it so the stack stops clean
		page := tab1.Get("/c/" + conv)
		assert.Equal(t, 200, page.Status)
	})

	t.Run("while the history catches up the page is whole at every moment, and only grows", func(t *testing.T) {
		c := s.SignedIn()
		conv := c.NewChat()
		reply := c.Send(conv, "quiero yapear")
		c.OpenStream(patient(), conv, reply).Drain()
		deadline := time.Now().Add(20 * time.Second)
		last, grew := 0, 0

		for time.Now().Before(deadline) {
			page := c.Get("/c/" + conv)
			require.Equal(t, 200, page.Status, "a conversation started here is never a 404 or an error while the history catches up")
			n := len(stack.MessagesIn(t, page.Body))
			require.GreaterOrEqual(t, n, last, "the page never loses a message it showed")
			if n > last {
				grew++
				t.Logf("the page shows %d messages after %s", n, time.Until(deadline).Round(time.Millisecond))
			}
			last = n
			if n >= 2 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		assert.GreaterOrEqual(t, last, 2, "the user message and the agent's reply reached the history")
		assert.Contains(t, c.Get("/history", "HX-Request", "true").Body, conv)
	})
}

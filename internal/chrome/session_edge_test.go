package chrome_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

func answers(text string) toolFunc {
	return func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult(text), nil }
}

func TestCall_ResultsThatAreNotTheUsualOnes(t *testing.T) {
	s, _ := connectInMemory(t, map[string]toolFunc{
		"empty_error": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true}, nil
		},
		"error_prefix_only": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: "), nil },
		"error_in_two_blocks": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			res := textResult("Error: first", "second")
			res.IsError = true
			return res, nil
		},
		"error_with_emoji": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return errorResult("Error: " + strings.Repeat("😀", 700)), nil
		},
		"images_only": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.ImageContent{MIMEType: "image/png", Data: []byte("one")},
				&mcp.ImageContent{MIMEType: "image/jpeg", Data: []byte("two")},
			}}, nil
		},
		"other_content": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.AudioContent{MIMEType: "audio/wav", Data: []byte("a")},
				&mcp.TextContent{Text: "only this"},
				&mcp.ResourceLink{URI: "file:///x", Name: "x"},
			}}, nil
		},
		"many_blocks": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			parts := make([]string, 1000)
			for i := range parts {
				parts[i] = "line " + strconv.Itoa(i)
			}
			return textResult(parts...), nil
		},
		"spaces": answers("  keep me \n\n"),
		"huge":   answers(strings.Repeat("é", 500_000)),
	}, chrome.Options{})

	t.Run("an error with nothing to say", func(t *testing.T) {
		_, err := s.Call(t.Context(), "empty_error", nil)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.Empty(t, toolErr.Message)
		assert.EqualError(t, err, "empty_error: ")
	})
	t.Run("an error that is only its prefix", func(t *testing.T) {
		_, err := s.Call(t.Context(), "error_prefix_only", nil)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.Empty(t, toolErr.Message)
	})
	t.Run("an error in two blocks says both", func(t *testing.T) {
		res, err := s.Call(t.Context(), "error_in_two_blocks", nil)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.Equal(t, "first second", toolErr.Message, "on one line")
		assert.Equal(t, "Error: first\nsecond", res.Text)
	})
	t.Run("a long message of emoji is cut at a character", func(t *testing.T) {
		_, err := s.Call(t.Context(), "error_with_emoji", nil)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.True(t, utf8.ValidString(toolErr.Message))
		assert.Equal(t, 501, utf8.RuneCountInString(toolErr.Message), "500 characters and the mark")
	})
	t.Run("pictures and no text", func(t *testing.T) {
		got, err := s.Call(t.Context(), "images_only", nil)

		require.NoError(t, err)
		assert.Empty(t, got.Text)
		assert.Equal(t, []chrome.Image{{MIMEType: "image/png", Data: []byte("one")}, {MIMEType: "image/jpeg", Data: []byte("two")}}, got.Images)
	})
	t.Run("content that is not text or a picture is left out", func(t *testing.T) {
		got, err := s.Call(t.Context(), "other_content", nil)

		require.NoError(t, err)
		assert.Equal(t, "only this", got.Text)
		assert.Empty(t, got.Images)
	})
	t.Run("a thousand blocks", func(t *testing.T) {
		got, err := s.Call(t.Context(), "many_blocks", nil)

		require.NoError(t, err)
		lines := strings.Split(got.Text, "\n")
		require.Len(t, lines, 1000)
		assert.Equal(t, "line 999", lines[999])
	})
	t.Run("the text is not trimmed", func(t *testing.T) {
		got, err := s.Call(t.Context(), "spaces", nil)

		require.NoError(t, err)
		assert.Equal(t, "  keep me \n\n", got.Text)
	})
	t.Run("a result of millions of characters", func(t *testing.T) {
		got, err := s.Call(t.Context(), "huge", nil)

		require.NoError(t, err)
		assert.Equal(t, 1_000_000, len(got.Text))
	})
	t.Run("through all of that the session is good", func(t *testing.T) {
		got, err := s.Call(t.Context(), "spaces", nil)

		require.NoError(t, err)
		assert.NotEmpty(t, got.Text)
	})
}

func TestCall_Arguments(t *testing.T) {
	s, _ := connectInMemory(t, map[string]toolFunc{
		"echo": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			return textResult(fmt.Sprint(args["value"])), nil
		},
	}, chrome.Options{})

	t.Run("text of every kind arrives as it was", func(t *testing.T) {
		for _, value := range []string{"", `say "hi"`, `back\slash`, "line\nbreak\ttab", "emoji 😀 日本語 שלום", "\u0000nul", strings.Repeat("x", 1<<20)} {
			got, err := s.Call(t.Context(), "echo", map[string]any{"value": value})

			require.NoError(t, err)
			assert.Equal(t, value, got.Text)
		}
	})
	t.Run("arguments that cannot be sent are an error of the call, not the end of the browser", func(t *testing.T) {
		_, err := s.Call(t.Context(), "echo", map[string]any{"value": make(chan int)})

		require.Error(t, err)
		assert.ErrorContains(t, err, "calling echo")
		assert.NotErrorIs(t, err, chrome.ErrDriverDied)
		got, err := s.Call(t.Context(), "echo", map[string]any{"value": "still here"})
		require.NoError(t, err, "the session is good")
		assert.Equal(t, "still here", got.Text)
	})
}

func TestCall_ContextAndTimeouts(t *testing.T) {
	var started atomicCounter
	s, _ := connectInMemory(t, map[string]toolFunc{
		"ok": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			started.add()
			return textResult("fine"), nil
		},
	}, chrome.Options{})

	t.Run("a context that is already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := s.Call(ctx, "ok", nil)

		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, chrome.ErrDriverDied)
		assert.ErrorContains(t, err, "calling ok")
		got, err := s.Call(t.Context(), "ok", nil)
		require.NoError(t, err, "the session is good")
		assert.Equal(t, "fine", got.Text)
	})
	t.Run("a context whose deadline has passed", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()

		_, err := s.Call(ctx, "ok", nil)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, chrome.ErrDriverDied)
		_, err = s.Call(t.Context(), "ok", nil)
		require.NoError(t, err, "the session is good")
	})
	// A timeout of nothing is not a call that has to be answered before it is made: that would end the session at once.
	for _, d := range []time.Duration{0, -time.Second} {
		t.Run("a timeout of "+d.String()+" means the session's", func(t *testing.T) {
			got, err := s.Call(t.Context(), "ok", nil, chrome.WithTimeout(d))

			require.NoError(t, err)
			assert.Equal(t, "fine", got.Text)
			_, err = s.Call(t.Context(), "ok", nil)
			require.NoError(t, err, "the session is good")
		})
	}
}

// atomicCounter counts the calls a fake tool served.
type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) add() { c.mu.Lock(); c.n++; c.mu.Unlock() }

func TestSession_ConcurrentCalls(t *testing.T) {
	// The session is meant for one driver, but a caller that overlaps calls (a watchdog taking a screenshot while a step is
	// waiting) must get each call its own answer, and the race detector must be quiet.
	s, _ := connectInMemory(t, map[string]toolFunc{
		"echo": func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			select {
			case <-time.After(time.Duration(int(args["n"].(float64))%5) * time.Millisecond):
			case <-ctx.Done():
			}
			return textResult("n=" + strconv.Itoa(int(args["n"].(float64)))), nil
		},
	}, chrome.Options{})
	const callers = 50
	results := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup

	for i := range callers {
		wg.Go(func() {
			res, err := s.Call(t.Context(), "echo", map[string]any{"n": i})
			results[i], errs[i] = res.Text, err
			_ = s.Info()
		})
	}
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		assert.Equal(t, "n="+strconv.Itoa(i), results[i], "each call gets its own answer")
	}
}

func TestSession_CloseFromSeveralPlacesAtOnce(t *testing.T) {
	s, _ := connectInMemory(t, map[string]toolFunc{"ok": answers("fine")}, chrome.Options{})
	errs := make(chan error, 10)
	var wg sync.WaitGroup

	for range 10 {
		wg.Go(func() { errs <- s.Close(t.Context()) })
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}
}

func TestSession_Info(t *testing.T) {
	t.Run("it is what the server said, and stays so after Close", func(t *testing.T) {
		s, _ := connectInMemory(t, map[string]toolFunc{"ok": answers("fine")}, chrome.Options{})
		want := chrome.ServerInfo{Name: "chrome_devtools", Version: "1.10.1"}
		assert.Equal(t, want, s.Info())

		require.NoError(t, s.Close(t.Context()))

		assert.Equal(t, want, s.Info())
	})
	t.Run("a client that has not been through the handshake has nothing to say", func(t *testing.T) {
		s := chrome.NewSessionForTest(&mcp.ClientSession{}, chrome.Options{})

		assert.Equal(t, chrome.ServerInfo{}, s.Info())
	})
}

func TestSession_AToolThatReturnsAnErrorOfItsOwn(t *testing.T) {
	s, _ := connectInMemory(t, map[string]toolFunc{
		"fails": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return nil, errors.New("handler blew up")
		},
		"ok": answers("fine"),
	}, chrome.Options{})

	_, err := s.Call(t.Context(), "fails", nil)

	require.Error(t, err)
	assert.NotErrorIs(t, err, chrome.ErrDriverDied, "the server answered")
	_, err = s.Call(t.Context(), "ok", nil)
	require.NoError(t, err)
}

func TestOutputOf_EdgeCases(t *testing.T) {
	t.Run("a nil buffer", func(t *testing.T) {
		assert.Empty(t, chrome.OutputOf(nil))
	})
	t.Run("output of only white space", func(t *testing.T) {
		b := chrome.NewTailBuffer(100)
		_, _ = b.Write([]byte(" \n\t\n "))

		assert.Empty(t, chrome.OutputOf(b))
	})
	t.Run("the end of a long output is cut at a character", func(t *testing.T) {
		b := chrome.NewTailBuffer(100_000)
		_, _ = b.Write([]byte(strings.Repeat("ñ😀 ", 20_000)))

		got := chrome.OutputOf(b)

		assert.True(t, utf8.ValidString(got))
		assert.Less(t, utf8.RuneCountInString(got), 700)
	})
	t.Run("a buffer that cut a character in half when it kept the end", func(t *testing.T) {
		b := chrome.NewTailBuffer(5)
		_, _ = b.Write([]byte("😀😀😀")) // 12 bytes; the last 5 start in the middle of the second emoji

		got := chrome.OutputOf(b)

		assert.True(t, utf8.ValidString(got), "what an error says is text: %q", got)
	})
}

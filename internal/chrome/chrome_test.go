package chrome_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

// app is a Web UI as far as the explorer can tell: pages the fake browser shows, and what was done to them.
type app struct {
	mu       sync.Mutex
	pages    map[string]string // url -> the accessibility tree the page shows
	current  string
	calls    []string
	filled   map[string]string
	loginErr bool
}

func (a *app) record(call string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

func (a *app) tools() map[string]toolFunc {
	return map[string]toolFunc{
		"new_page": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			a.record("new_page " + args["url"].(string))
			a.current = args["url"].(string)
			return textResult("## Pages\n1: Sign in (" + a.current + ") [selected]"), nil
		},
		"emulate": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("ok"), nil },
		"navigate_page": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			a.record("navigate " + args["url"].(string))
			a.current = args["url"].(string)
			return textResult("## Pages\n1: Chat (" + a.current + ") [selected]"), nil
		},
		"take_snapshot": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			return textResult(a.pages[a.current]), nil
		},
		"fill": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.filled == nil {
				a.filled = map[string]string{}
			}
			a.filled[args["uid"].(string)] = args["value"].(string)
			return textResult("filled"), nil
		},
		"click": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			a.record("click " + args["uid"].(string))
			if args["uid"] == "1_3" { // Sign in
				a.mu.Lock()
				defer a.mu.Unlock()
				if !a.loginErr {
					a.current = "http://app/"
				}
			}
			return textResult("clicked"), nil
		},
		"evaluate_script": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return textResult("Script ran on page and returned:\n```json\n\"x\"\n```"), nil
		},
		"take_screenshot": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			res := textResult("shot")
			res.Content = append(res.Content, &mcp.ImageContent{Data: []byte("PNGDATA"), MIMEType: "image/png"})
			return res, nil
		},
	}
}

const signInPage = `uid=1_0 RootWebArea "Sign in"
  uid=1_1 textbox "Email" focusable required
  uid=1_2 textbox "Password" required
  uid=1_3 button "Sign in"
`

const chatPage = `uid=2_0 RootWebArea "New chat"
  uid=2_1 textbox "Message" focusable multiline required
  uid=2_2 button "Send message"
`

func newChrome(t *testing.T, a *app) *chrome.Chrome {
	t.Helper()
	s, _ := connectInMemory(t, a.tools(), chrome.Options{})
	c := chrome.NewChrome(s, "http://app/", chrome.ChromeConfig{Poll: time.Millisecond, Settle: 200 * time.Millisecond})
	return c
}

func TestChrome_LoginFillsTheFormAndWaitsForTheComposer(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
	c := newChrome(t, a)

	require.NoError(t, c.Login(t.Context(), "admin@example.com", "pw"))

	assert.Equal(t, "admin@example.com", a.filled["1_1"])
	assert.Equal(t, "pw", a.filled["1_2"])
	assert.Contains(t, a.calls, "new_page http://app/login")
	assert.Contains(t, a.calls, "click 1_3")
}

func TestChrome_LoginThatDoesNotFinishIsErrLoginFailedAndSaysNothingOfThePassword(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage}, loginErr: true}
	c := newChrome(t, a)

	err := c.Login(t.Context(), "admin@example.com", "hunter2-secret")

	require.ErrorIs(t, err, chrome.ErrLoginFailed)
	assert.NotContains(t, err.Error(), "hunter2-secret")
}

func TestChrome_StartChatOpensTheStartPageAndWaitsForTheComposer(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
	c := newChrome(t, a)
	require.NoError(t, c.Login(t.Context(), "a@b.c", "pw"))

	require.NoError(t, c.StartChat(t.Context()))

	assert.Contains(t, a.calls, "navigate http://app/")
}

func TestChrome_StartChatOnASignedOutPageSaysTheSessionEnded(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
	c := newChrome(t, a)
	require.NoError(t, c.Login(t.Context(), "a@b.c", "pw"))
	a.mu.Lock()
	a.pages["http://app/"] = signInPage // the session has a lifetime: the start page now asks to sign in
	a.mu.Unlock()

	err := c.StartChat(t.Context())

	require.ErrorIs(t, err, chrome.ErrSessionEnded)
}

func TestChrome_ClickFillEvalAndToolGoThroughTheTab(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
	c := newChrome(t, a)
	require.NoError(t, c.Login(t.Context(), "a@b.c", "pw"))
	require.NoError(t, c.StartChat(t.Context()))

	require.NoError(t, c.Fill(t.Context(), chrome.Query{Role: "textbox", Name: "Message"}, "hola"))
	require.NoError(t, c.Click(t.Context(), chrome.Query{Role: "button", Name: "Send message"}))
	var got string
	require.NoError(t, c.Eval(t.Context(), "() => 'x'", &got))
	res, err := c.Tool(t.Context(), "take_snapshot", nil)
	require.NoError(t, err)

	assert.Equal(t, "hola", a.filled["2_1"])
	assert.Contains(t, a.calls, "click 2_2")
	assert.Equal(t, "x", got)
	assert.Contains(t, res.Text, "New chat")
}

func TestChrome_ScreenshotSavesThePicturePrivately(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
	c := newChrome(t, a)
	require.NoError(t, c.Login(t.Context(), "a@b.c", "pw"))
	path := filepath.Join(t.TempDir(), "shot.png")

	require.NoError(t, c.Screenshot(t.Context(), path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "PNGDATA", string(data))
	info, _ := os.Stat(path)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestChrome_ScreenshotWithoutAPictureIsAnError(t *testing.T) {
	a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
	tools := a.tools()
	tools["take_screenshot"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("shot"), nil }
	s, _ := connectInMemory(t, tools, chrome.Options{})
	c := chrome.NewChrome(s, "http://app/", chrome.ChromeConfig{})
	require.NoError(t, c.Login(t.Context(), "a", "b"))

	err := c.Screenshot(t.Context(), filepath.Join(t.TempDir(), "x.png"))

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no picture"), err.Error())
}

func TestChrome_ALoginThatTheBrowserCannotStartIsReportedWithTheAddressWithoutCredentials(t *testing.T) {
	tools := map[string]toolFunc{
		"new_page": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return nil, errors.New("net::ERR_CONNECTION_REFUSED")
		},
	}
	s, _ := connectInMemory(t, tools, chrome.Options{})
	c := chrome.NewChrome(s, "http://user:pw-secret@app/", chrome.ChromeConfig{})

	err := c.Login(t.Context(), "a", "b")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "pw-secret")
}

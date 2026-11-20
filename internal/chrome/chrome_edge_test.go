package chrome_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

const blankPage = `uid=3_0 RootWebArea "Loading"
`

const alertPage = `uid=1_0 RootWebArea "Sign in"
  uid=1_1 textbox "Email" focusable required
  uid=1_2 textbox "Password" required
  uid=1_3 button "Sign in"
  uid=1_4 alert "Invalid email or password"
`

var fastConfig = chrome.ChromeConfig{Poll: time.Millisecond, Settle: 150 * time.Millisecond}

// signedIn is a chrome that has gone through Login on the pages of a.
func signedIn(t *testing.T, a *app, tools map[string]toolFunc) *chrome.Chrome {
	t.Helper()
	if tools == nil {
		tools = a.tools()
	}
	s, _ := connectInMemory(t, tools, chrome.Options{})
	c := chrome.NewChrome(s, "http://app/", fastConfig)
	require.NoError(t, c.Login(t.Context(), "a@b.c", "pw"))
	return c
}

func newApp() *app {
	return &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
}

func TestChrome_LoginPages(t *testing.T) {
	tests := []struct {
		name    string
		login   string // what /login shows
		wantErr string
		wantIs  error
	}{
		{"a page with an alert in place of the composer", alertPage, "", chrome.ErrLoginFailed},
		{"a page that is already the chat, as when the session is still good", chatPage, "", nil},
		{"a sign-in page with no password field", "uid=1_0 RootWebArea \"x\"\n  uid=1_1 textbox \"Email\"\n  uid=1_3 button \"Sign in\"\n", `the sign-in page: the page has no textbox named "Password" (it has: textbox "Email")`, nil},
		{"a sign-in page with no email field", "uid=1_0 RootWebArea \"x\"\n  uid=1_2 textbox \"Password\"\n  uid=1_3 button \"Sign in\"\n", `the sign-in page: the page has no textbox named "Email"`, nil},
		{"a sign-in page with no button", "uid=1_0 RootWebArea \"x\"\n  uid=1_1 textbox \"Email\"\n  uid=1_2 textbox \"Password\"\n", `pressing Sign in: the page has no button named "Sign in"`, nil},
		{"a page that is not there", "", "the page snapshot has no elements", nil},
		{"a page that is something else", blankPage, `the sign-in page: the page has no textbox named "Email"`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{pages: map[string]string{"http://app/login": tc.login, "http://app/": chatPage}, loginErr: true}
			s, _ := connectInMemory(t, a.tools(), chrome.Options{})
			c := chrome.NewChrome(s, "http://app/", fastConfig)

			err := c.Login(t.Context(), "admin@example.com", "hunter2-secret")

			switch {
			case tc.wantIs != nil:
				require.ErrorIs(t, err, tc.wantIs)
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
			default:
				require.NoError(t, err)
				assert.Empty(t, a.filled, "a page that is signed in is not asked for the password")
			}
			if err != nil {
				assert.NotContains(t, err.Error(), "hunter2-secret")
			}
		})
	}
}

func TestChrome_LoginTypesWhatItIsGivenAsItIs(t *testing.T) {
	long := strings.Repeat("ñ", 100_000)
	tests := []struct {
		name            string
		email, password string
	}{
		{"quotes and backslashes", `o"brien+a\b@example.com`, `p"a'ss\w'o"rd`},
		{"unicode and emoji", "josé@ejemplo.pe", "contraseña-😀-日本語"},
		{"right to left", "שלום@example.com", "كلمة السر"},
		{"new lines and tabs", "a@b.c", "line1\nline2\ttab"},
		{"spaces around", "  a@b.c  ", "  pw  "},
		{"empty", "", ""},
		{"very long", long + "@example.com", long},
		{"the password of a url", "a@b.c", "http://user:pw@host/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newApp()
			s, _ := connectInMemory(t, a.tools(), chrome.Options{})
			c := chrome.NewChrome(s, "http://app/", fastConfig)

			require.NoError(t, c.Login(t.Context(), tc.email, tc.password))

			assert.Equal(t, tc.email, a.filled["1_1"])
			assert.Equal(t, tc.password, a.filled["1_2"])
		})
	}
	t.Run("a refusal of the browser does not repeat them", func(t *testing.T) {
		a := newApp()
		tools := a.tools()
		tools["fill"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return errorResult("Error: the element is not editable"), nil
		}
		s, _ := connectInMemory(t, tools, chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", fastConfig)

		err := c.Login(t.Context(), "a@b.c", "hunter2-secret")

		require.ErrorContains(t, err, "typing the email")
		assert.NotContains(t, err.Error(), "hunter2-secret")
	})
	t.Run("a refusal to type the password does not repeat it", func(t *testing.T) {
		a := newApp()
		tools := a.tools()
		var calls atomic.Int32
		tools["fill"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			if calls.Add(1) == 2 {
				return errorResult("Error: the element is not editable"), nil
			}
			return textResult("filled"), nil
		}
		s, _ := connectInMemory(t, tools, chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", fastConfig)

		err := c.Login(t.Context(), "a@b.c", "hunter2-secret")

		require.ErrorContains(t, err, "typing the password")
		assert.NotContains(t, err.Error(), "hunter2-secret")
	})
}

func TestChrome_LoginOpeningTheBrowser(t *testing.T) {
	t.Run("a browser that reports no page", func(t *testing.T) {
		a := newApp()
		tools := a.tools()
		tools["new_page"] = answers("## Pages\n1: about:blank")
		s, _ := connectInMemory(t, tools, chrome.Options{})

		err := chrome.NewChrome(s, "http://app/", fastConfig).Login(t.Context(), "a", "b")

		require.ErrorContains(t, err, "the browser reported no page")
	})
	t.Run("a window that cannot be sized", func(t *testing.T) {
		a := newApp()
		tools := a.tools()
		tools["emulate"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return errorResult("Error: nope"), nil
		}
		s, _ := connectInMemory(t, tools, chrome.Options{})

		err := chrome.NewChrome(s, "http://app/", fastConfig).Login(t.Context(), "a", "b")

		require.ErrorContains(t, err, "setting the size of the browser window")
	})
	t.Run("the size asked for", func(t *testing.T) {
		for config, want := range map[string]string{"": chrome.DefaultViewport, "390x844x3,mobile": "390x844x3,mobile"} {
			a := newApp()
			tools := a.tools()
			var viewport atomic.Value
			tools["emulate"] = func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				viewport.Store(args["viewport"])
				return textResult("ok"), nil
			}
			s, _ := connectInMemory(t, tools, chrome.Options{})

			require.NoError(t, chrome.NewChrome(s, "http://app/", chrome.ChromeConfig{Viewport: config, Poll: time.Millisecond}).Login(t.Context(), "a", "b"))

			assert.Equal(t, want, viewport.Load())
		}
	})
	t.Run("the tab the browser opened is the one that is driven", func(t *testing.T) {
		a := newApp()
		var mu sync.Mutex
		var seen []string
		tools := a.tools()
		for name, fn := range tools {
			tools[name] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				mu.Lock()
				seen = append(seen, fmt.Sprintf("%s page=%v", name, args["pageId"]))
				mu.Unlock()
				if name == "new_page" {
					a.record("new_page " + args["url"].(string))
					a.current = args["url"].(string)
					return textResult("## Pages\n1: about:blank\n7: Sign in (" + a.current + ") [selected]"), nil
				}
				return fn(ctx, args)
			}
		}
		s, _ := connectInMemory(t, tools, chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", fastConfig)

		require.NoError(t, c.Login(t.Context(), "a", "b"))
		require.NoError(t, c.StartChat(t.Context()))

		mu.Lock()
		defer mu.Unlock()
		for _, call := range seen {
			if !strings.HasPrefix(call, "new_page") {
				assert.True(t, strings.HasSuffix(call, "page=7"), "%s: every call after the page was opened names it", call)
			}
		}
		assert.NotEmpty(t, seen)
	})
	t.Run("signing in twice opens a tab each time and drives the second", func(t *testing.T) {
		a := newApp()
		var opened atomic.Int32
		var lastPage atomic.Value
		tools := a.tools()
		inner := tools["new_page"]
		tools["new_page"] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			n := opened.Add(1)
			_, _ = inner(ctx, args)
			return textResult(fmt.Sprintf("## Pages\n%d: Sign in (http://app/login) [selected]", n)), nil
		}
		snapshot := tools["take_snapshot"]
		tools["take_snapshot"] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			lastPage.Store(args["pageId"])
			return snapshot(ctx, args)
		}
		s, _ := connectInMemory(t, tools, chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", fastConfig)

		require.NoError(t, c.Login(t.Context(), "a", "b"))
		a.mu.Lock()
		a.pages["http://app/"] = chatPage
		a.mu.Unlock()
		require.NoError(t, c.Login(t.Context(), "a", "b"))

		assert.EqualValues(t, 2, opened.Load())
		assert.EqualValues(t, 2, lastPage.Load(), "the second tab")
	})
}

func TestChrome_TheAddressOfTheWebUI(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		wantURL string
	}{
		{"a trailing slash", "http://app/", "http://app/login"},
		{"many trailing slashes", "http://app///", "http://app/login"},
		{"no trailing slash", "http://app", "http://app/login"},
		{"a port and a path", "http://app:8300/ui/", "http://app:8300/ui/login"},
		{"upper case is left as it is", "HTTP://APP/", "HTTP://APP/login"},
		{"credentials go to the browser, which needs them", "http://user:pw-secret@app/", "http://user:pw-secret@app/login"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var opened atomic.Value
			tools := newApp().tools()
			tools["new_page"] = func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				opened.Store(args["url"])
				return nil, errors.New("net::ERR_CONNECTION_REFUSED")
			}
			s, _ := connectInMemory(t, tools, chrome.Options{})

			err := chrome.NewChrome(s, tc.base, fastConfig).Login(t.Context(), "a", "b")

			require.Error(t, err)
			assert.Equal(t, tc.wantURL, opened.Load())
			assert.NotContains(t, err.Error(), "pw-secret", "no error shows the credentials")
		})
	}
	t.Run("a start page is the base with one slash", func(t *testing.T) {
		a := &app{pages: map[string]string{"http://app/login": signInPage, "http://app/": chatPage}}
		s, _ := connectInMemory(t, a.tools(), chrome.Options{})
		c := chrome.NewChrome(s, "http://app//", fastConfig)
		require.NoError(t, c.Login(t.Context(), "a", "b"))

		require.NoError(t, c.StartChat(t.Context()))

		assert.Contains(t, a.calls, "navigate http://app/")
	})
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    string
	}{
		{"a plain address", "http://app:8300", "http://app:8300"},
		{"a user and a password", "http://user:pw-secret@app:8300/x", "http://app:8300/x"},
		{"a user alone", "http://user@app/", "http://app/"},
		{"a password alone", "http://:pw-secret@app/", "http://app/"},
		{"an encoded password", "http://user:p%40ss%2Fw@app/", "http://app/"},
		{"a token in the query", "http://app/?token=pw-secret&x=1", "http://app/"},
		{"a token in the fragment", "http://app/#access_token=pw-secret", "http://app/"},
		{"an address that is not valid, with credentials", "http://user:pw-secret@[::1", "(an address)"},
		{"an address that is not valid", "http://[::1", "(an address)"},
		{"a bad escape", "http://user:p%zzw@app/", "(an address)"},
		{"a control character", "http://app/\x7f", "(an address)"},
		{"nothing", "", "(an address)"},
		{"no scheme", "user:pw-secret@app", "(an address)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chrome.RedactURL(tc.address)

			assert.NotContains(t, got, "pw-secret")
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestChrome_NothingWorksBeforeLoginHasOpenedATab(t *testing.T) {
	var served atomic.Int32
	tools := newApp().tools()
	for name, fn := range tools {
		tools[name] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			served.Add(1)
			return fn(ctx, args)
		}
	}
	s, _ := connectInMemory(t, tools, chrome.Options{})
	c := chrome.NewChrome(s, "http://app/", fastConfig)
	ctx := t.Context()
	var out string

	calls := map[string]error{
		"StartChat":  c.StartChat(ctx),
		"Click":      c.Click(ctx, chrome.Query{Role: "button"}),
		"Fill":       c.Fill(ctx, chrome.Query{Role: "textbox"}, "x"),
		"Eval":       c.Eval(ctx, "() => 1", &out),
		"Screenshot": c.Screenshot(ctx, filepath.Join(t.TempDir(), "x.png")),
	}
	_, calls["Tool"] = c.Tool(ctx, "take_snapshot", nil)

	for name, err := range calls {
		require.ErrorIs(t, err, chrome.ErrNoTab, name)
		assert.ErrorContains(t, err, "Login", name+": it says what to do")
	}
	assert.Zero(t, served.Load(), "and the browser was not asked to act on a page that is not there")
}

func TestChrome_StartChat(t *testing.T) {
	t.Run("a page that shows neither the composer nor the sign-in form is an error in time", func(t *testing.T) {
		a := newApp()
		c := signedIn(t, a, nil)
		a.mu.Lock()
		a.pages["http://app/"] = blankPage
		a.mu.Unlock()
		start := time.Now()

		err := c.StartChat(t.Context())

		require.ErrorContains(t, err, "waiting for the composer")
		assert.Less(t, time.Since(start), 5*time.Second)
		assert.NotErrorIs(t, err, chrome.ErrSessionEnded)
	})
	t.Run("a page that takes a moment", func(t *testing.T) {
		a := newApp()
		var looks atomic.Int32
		tools := a.tools()
		tools["take_snapshot"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.current == "http://app/" && looks.Add(1) < 4 {
				return textResult(""), nil // the tool had nothing yet: not an error of the run
			}
			return textResult(a.pages[a.current]), nil
		}
		c := signedIn(t, a, tools)

		require.NoError(t, c.StartChat(t.Context()))
	})
	t.Run("the browser cannot open the start page", func(t *testing.T) {
		a := newApp()
		tools := a.tools()
		tools["navigate_page"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return errorResult("Error: net::ERR_CONNECTION_REFUSED"), nil
		}
		c := signedIn(t, a, tools)

		err := c.StartChat(t.Context())

		require.ErrorContains(t, err, "opening the start page")
		require.ErrorContains(t, err, "ERR_CONNECTION_REFUSED")
	})
	t.Run("a run that is canceled while the page is awaited stops at once", func(t *testing.T) {
		a := newApp()
		s, _ := connectInMemory(t, a.tools(), chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", chrome.ChromeConfig{Poll: time.Millisecond, Settle: time.Hour})
		require.NoError(t, c.Login(t.Context(), "a", "b"))
		a.mu.Lock()
		a.pages["http://app/"] = blankPage
		a.mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()

		err := c.StartChat(ctx)

		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("a login that is canceled while the composer is awaited stops at once", func(t *testing.T) {
		a := &app{pages: map[string]string{"http://app/login": signInPage}, loginErr: true}
		s, _ := connectInMemory(t, a.tools(), chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", chrome.ChromeConfig{Poll: time.Millisecond, Settle: time.Hour})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()

		err := c.Login(ctx, "a", "b")

		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("the wait is as long as the settle time and no longer, whatever the poll", func(t *testing.T) {
		a := newApp()
		s, _ := connectInMemory(t, a.tools(), chrome.Options{})
		c := chrome.NewChrome(s, "http://app/", chrome.ChromeConfig{Poll: time.Hour, Settle: 100 * time.Millisecond})
		require.NoError(t, c.Login(t.Context(), "a", "b"))
		a.mu.Lock()
		a.pages["http://app/"] = blankPage
		a.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		err := c.StartChat(ctx)

		require.ErrorContains(t, err, "waiting for the composer")
		assert.NoError(t, ctx.Err(), "it gave up by itself, not because the test's patience ended")
	})
}

func TestChrome_NavigateGivesTheInitScriptToTheBrowser(t *testing.T) {
	a := newApp()
	var args atomic.Value
	tools := a.tools()
	inner := tools["navigate_page"]
	tools["navigate_page"] = func(ctx context.Context, got map[string]any) (*mcp.CallToolResult, error) {
		args.Store(got)
		return inner(ctx, got)
	}
	c := signedIn(t, a, tools)

	require.NoError(t, chrome.Navigate(c, t.Context(), "http://app/", "window.x = 1"))
	assert.Equal(t, "window.x = 1", args.Load().(map[string]any)["initScript"])
	require.NoError(t, chrome.Navigate(c, t.Context(), "http://app/", ""))
	assert.NotContains(t, args.Load().(map[string]any), "initScript", "no script, no argument")
}

func TestChrome_StartChatWhenThePageCannotBeReadOnceItIsThere(t *testing.T) {
	a := newApp()
	var navigated atomic.Bool
	var looks atomic.Int32
	tools := a.tools()
	navigate := tools["navigate_page"]
	tools["navigate_page"] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		navigated.Store(true)
		return navigate(ctx, args)
	}
	snapshot := tools["take_snapshot"]
	tools["take_snapshot"] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		if navigated.Load() && looks.Add(1) == 2 {
			return errorResult("Error: the page closed"), nil // the look after the wait
		}
		return snapshot(ctx, args)
	}
	c := signedIn(t, a, tools)

	err := c.StartChat(t.Context())

	require.ErrorContains(t, err, "the page closed")
}

func TestChrome_FillAndClickSayWhatThePageHad(t *testing.T) {
	a := newApp()
	c := signedIn(t, a, nil)
	require.NoError(t, c.StartChat(t.Context()))

	t.Run("fill", func(t *testing.T) {
		err := c.Fill(t.Context(), chrome.Query{Role: "textbox", Name: "Nope"}, "x")

		var nf *chrome.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.EqualError(t, err, `the page has no textbox named "Nope" (it has: textbox "Message")`)
	})
	t.Run("click", func(t *testing.T) {
		err := c.Click(t.Context(), chrome.Query{Role: "button", Name: "Nope"})

		var nf *chrome.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.EqualError(t, err, `the page has no button named "Nope" (it has: button "Send message")`)
	})
	t.Run("fill when the snapshot cannot be read", func(t *testing.T) {
		a.mu.Lock()
		a.pages["http://app/"] = ""
		a.mu.Unlock()

		err := c.Fill(t.Context(), chrome.Query{Role: "textbox", Name: "Message"}, "x")

		require.ErrorContains(t, err, "the page snapshot has no elements")
	})
}

func TestChrome_ClickTriesAgainOnAPageThatChanged(t *testing.T) {
	stale := func(uid string) *mcp.CallToolResult {
		return errorResult(`Error: Element uid "` + uid + `" not found on page 1.`)
	}
	build := func(t *testing.T, click func(n int, uid string) *mcp.CallToolResult) (*chrome.Chrome, *atomic.Int32, *atomic.Int32) {
		t.Helper()
		a := newApp()
		var clicks, snapshots atomic.Int32
		tools := a.tools()
		snapshot := tools["take_snapshot"]
		tools["take_snapshot"] = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			snapshots.Add(1)
			return snapshot(ctx, args)
		}
		tools["click"] = func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if args["uid"] == "1_3" {
				a.current = "http://app/"
				return textResult("clicked"), nil
			}
			return click(int(clicks.Add(1)), args["uid"].(string)), nil
		}
		return signedIn(t, a, tools), &clicks, &snapshots
	}
	query := chrome.Query{Role: "button", Name: "Send message"}

	t.Run("a uid that is gone is looked up again, once", func(t *testing.T) {
		c, clicks, snapshots := build(t, func(n int, uid string) *mcp.CallToolResult {
			if n == 1 {
				return stale(uid)
			}
			return textResult("clicked")
		})
		require.NoError(t, c.StartChat(t.Context()))
		before := snapshots.Load()

		require.NoError(t, c.Click(t.Context(), query))

		assert.EqualValues(t, 2, clicks.Load())
		assert.EqualValues(t, 2, snapshots.Load()-before, "a new snapshot for the second try")
	})
	t.Run("a uid that is gone twice is an error, and not a third try", func(t *testing.T) {
		c, clicks, _ := build(t, func(_ int, uid string) *mcp.CallToolResult { return stale(uid) })
		require.NoError(t, c.StartChat(t.Context()))

		err := c.Click(t.Context(), query)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.Contains(t, toolErr.Message, "not found")
		assert.EqualValues(t, 2, clicks.Load())
	})
	t.Run("another refusal is not tried again", func(t *testing.T) {
		c, clicks, _ := build(t, func(int, string) *mcp.CallToolResult { return errorResult("Error: the element is covered by another") })
		require.NoError(t, c.StartChat(t.Context()))

		err := c.Click(t.Context(), query)

		require.ErrorContains(t, err, "covered by another")
		assert.EqualValues(t, 1, clicks.Load())
	})
	t.Run("a page that went away between the tries", func(t *testing.T) {
		a := newApp()
		var clicks atomic.Int32
		tools := a.tools()
		tools["click"] = func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if args["uid"] == "1_3" {
				a.current = "http://app/"
				return textResult("clicked"), nil
			}
			clicks.Add(1)
			a.pages["http://app/"] = "" // the page is gone when the second snapshot is taken
			return stale(args["uid"].(string)), nil
		}
		c := signedIn(t, a, tools)
		require.NoError(t, c.StartChat(t.Context()))

		err := c.Click(t.Context(), query)

		require.ErrorContains(t, err, "the page snapshot has no elements")
		assert.EqualValues(t, 1, clicks.Load())
	})
	t.Run("what counts as a stale element", func(t *testing.T) {
		assert.True(t, chrome.IsStale(&chrome.ToolError{Tool: "click", Message: `Element uid "9_99" not found on page 2.`}))
		assert.True(t, chrome.IsStale(fmt.Errorf("pressing: %w", &chrome.ToolError{Tool: "click", Message: "not found"})))
		assert.False(t, chrome.IsStale(&chrome.ToolError{Tool: "click", Message: "covered"}))
		assert.False(t, chrome.IsStale(errors.New("not found")), "an error that is not a tool's own")
		assert.False(t, chrome.IsStale(nil))
		assert.False(t, chrome.IsStale(&chrome.NotFoundError{}))
	})
}

func TestChrome_Eval(t *testing.T) {
	a := newApp()
	tools := a.tools()
	results := map[string]string{
		"() => 'x'":       "Script ran on page and returned:\n```json\n\"x\"\n```",
		"() => nothing":   "Script ran on page and returned:\n```json\n\n```",
		"() => number":    "Script ran on page and returned:\n```json\n42\n```",
		"() => nofence":   "Script ran on page and returned nothing",
		"() => undefined": "Script ran on page and returned:\n```json\nundefined\n```",
	}
	tools["evaluate_script"] = func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		function := args["function"].(string)
		if function == "() => throws" {
			return errorResult("Error: boom"), nil
		}
		if text, ok := results[function]; ok {
			return textResult(text), nil
		}
		return textResult("Script ran on page and returned:\n```json\n\"x\"\n```"), nil
	}
	c := signedIn(t, a, tools)

	t.Run("a null result into a string is the empty string", func(t *testing.T) {
		got := "before"

		require.NoError(t, c.Eval(t.Context(), "() => nothing", &got))

		assert.Empty(t, got)
	})
	t.Run("no destination", func(t *testing.T) {
		require.NoError(t, c.Eval(t.Context(), "() => 'x'", nil))
	})
	t.Run("a result of another type than the destination's names the script", func(t *testing.T) {
		var got string

		err := c.Eval(t.Context(), "() => number", &got)

		require.ErrorContains(t, err, "() => number: reading the result")
	})
	t.Run("a script that throws names the script", func(t *testing.T) {
		var got string

		err := c.Eval(t.Context(), "() => throws", &got)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.EqualError(t, err, "() => throws: evaluate_script: boom")
	})
	t.Run("a tool that printed no result", func(t *testing.T) {
		err := c.Eval(t.Context(), "() => nofence", new(string))

		require.ErrorIs(t, err, chrome.ErrNoScriptResult)
		assert.ErrorContains(t, err, "() => nofence")
	})
	t.Run("a script of many lines names its first only", func(t *testing.T) {
		err := c.Eval(t.Context(), "() => {\n  return document.cookie\n}", new(int))

		require.Error(t, err)
		assert.ErrorContains(t, err, "() => {: reading the result")
		assert.NotContains(t, err.Error(), "document.cookie", "the body of a script is not in an error")
	})
	t.Run("undefined cannot be read into a value", func(t *testing.T) {
		err := c.Eval(t.Context(), "() => undefined", new(string))

		require.ErrorContains(t, err, "reading the result")
		assert.NoError(t, c.Eval(t.Context(), "() => undefined", nil), "unless nobody wants it")
	})
}

func TestChrome_ToolDoesNotChangeTheArgumentsItIsGiven(t *testing.T) {
	a := newApp()
	c := signedIn(t, a, nil)
	args := map[string]any{"key": "Enter"}

	_, err := c.Tool(t.Context(), "take_snapshot", args)

	require.NoError(t, err)
	assert.Equal(t, map[string]any{"key": "Enter"}, args, "the tab's id went to the browser, not into the caller's map")
}

func TestChrome_Screenshot(t *testing.T) {
	pictures := func(images ...*mcp.ImageContent) toolFunc {
		return func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			res := textResult("shot")
			for _, img := range images {
				res.Content = append(res.Content, img)
			}
			return res, nil
		}
	}
	chromeWith := func(t *testing.T, shot toolFunc) *chrome.Chrome {
		t.Helper()
		a := newApp()
		tools := a.tools()
		tools["take_screenshot"] = shot
		return signedIn(t, a, tools)
	}
	png := &mcp.ImageContent{MIMEType: "image/png", Data: []byte("PNGDATA")}

	t.Run("a directory that is not there", func(t *testing.T) {
		c := chromeWith(t, pictures(png))
		path := filepath.Join(t.TempDir(), "missing", "dir", "shot.png")

		err := c.Screenshot(t.Context(), path)

		require.ErrorIs(t, err, os.ErrNotExist)
		assert.NoFileExists(t, path)
	})
	t.Run("a path that is a directory", func(t *testing.T) {
		c := chromeWith(t, pictures(png))

		err := c.Screenshot(t.Context(), t.TempDir())

		require.Error(t, err)
	})
	t.Run("a directory that cannot be written", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		c := chromeWith(t, pictures(png))

		err := c.Screenshot(t.Context(), filepath.Join(dir, "shot.png"))

		require.ErrorIs(t, err, os.ErrPermission)
	})
	t.Run("a picture of no bytes is not a picture", func(t *testing.T) {
		c := chromeWith(t, pictures(&mcp.ImageContent{MIMEType: "image/png", Data: []byte{}}))
		path := filepath.Join(t.TempDir(), "shot.png")

		err := c.Screenshot(t.Context(), path)

		require.Error(t, err)
		assert.ErrorContains(t, err, "no picture")
		assert.NoFileExists(t, path, "an empty file would be taken for a screenshot")
	})
	t.Run("an empty picture first and a good one after", func(t *testing.T) {
		c := chromeWith(t, pictures(&mcp.ImageContent{MIMEType: "image/png", Data: []byte{}}, png))
		path := filepath.Join(t.TempDir(), "shot.png")

		require.NoError(t, c.Screenshot(t.Context(), path))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "PNGDATA", string(data))
	})
	t.Run("a file that is there is replaced", func(t *testing.T) {
		c := chromeWith(t, pictures(png))
		path := filepath.Join(t.TempDir(), "shot.png")
		require.NoError(t, os.WriteFile(path, []byte("old and longer than the new one"), 0o600))

		require.NoError(t, c.Screenshot(t.Context(), path))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "PNGDATA", string(data))
	})
	t.Run("a tool that refuses", func(t *testing.T) {
		c := chromeWith(t, func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return errorResult("Error: no page"), nil
		})
		path := filepath.Join(t.TempDir(), "shot.png")

		err := c.Screenshot(t.Context(), path)

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.NoFileExists(t, path)
	})
}

func TestChrome_Close(t *testing.T) {
	a := newApp()
	c := signedIn(t, a, nil)

	require.NoError(t, c.Close(t.Context()))
	require.NoError(t, c.Close(t.Context()), "twice is fine")

	_, err := c.Tool(t.Context(), "take_snapshot", nil)
	require.ErrorIs(t, err, chrome.ErrDriverDied)
	require.ErrorIs(t, c.StartChat(t.Context()), chrome.ErrDriverDied)
	require.ErrorIs(t, c.Login(t.Context(), "a", "b"), chrome.ErrDriverDied)
}

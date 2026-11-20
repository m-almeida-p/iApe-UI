package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
	"iape-ui/internal/platform/envcfg"
)

// fakeSession is a page that shows what a path's expectations ask for.
type fakeSession struct {
	text     string
	cards    []explore.Card
	fresh    int
	closed   bool
	failType bool
}

func (f *fakeSession) Type(context.Context, string) error {
	if f.failType {
		return errors.New("the composer is gone")
	}
	return nil
}
func (f *fakeSession) Send(context.Context) error                    { return nil }
func (f *fakeSession) Pick(context.Context, string) error            { return nil }
func (f *fakeSession) Click(context.Context, string) (bool, error)   { return true, nil }
func (f *fakeSession) FillOTP(context.Context, string) error         { return nil }
func (f *fakeSession) Settle(context.Context) error                  { return nil }
func (f *fakeSession) Text(context.Context) (string, error)          { return f.text, nil }
func (f *fakeSession) Cards(context.Context) ([]explore.Card, error) { return f.cards, nil }
func (f *fakeSession) Requests(context.Context) (string, error)      { return "GET / [200]", nil }
func (f *fakeSession) Screenshot(_ context.Context, path string) error {
	return os.WriteFile(path, []byte("png"), 0o600)
}
func (f *fakeSession) Fresh(context.Context) error { f.fresh++; return nil }
func (f *fakeSession) Close(context.Context) error { f.closed = true; return nil }

func testSettings() settings {
	return settings{
		BaseURL: "http://localhost:8300", Command: []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"},
		Args:  []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"},
		Email: "admin@example.com", Password: envcfg.Secret("hunter2-secret"),
	}
}

type harness struct {
	out, err   bytes.Buffer
	sess       *fakeSession
	opened     int
	openedWith openOptions
	wrap       func(*fakeSession) session // when it is set, the session the explorer is given
	openErr    error
	cfgErr     error
	cfg        settings
	dir        string
}

func newHarness(t *testing.T, sess *fakeSession) *harness {
	t.Helper()
	return &harness{sess: sess, cfg: testSettings(), dir: filepath.Join(t.TempDir(), "out")}
}

func (h *harness) run(args ...string) int {
	d := deps{
		stdout: &h.out, stderr: &h.err,
		loadSettings: func(string) (settings, error) { return h.cfg, h.cfgErr },
		open: func(_ context.Context, _ settings, o openOptions) (session, error) {
			h.opened++
			h.openedWith = o
			if h.wrap != nil {
				return h.wrap(h.sess), h.openErr
			}
			return h.sess, h.openErr
		},
		now: func() time.Time { return time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC) },
	}
	return d.run(context.Background(), append([]string{"-out", h.dir}, args...))
}

func TestRun_List(t *testing.T) {
	h := newHarness(t, &fakeSession{})

	code := h.run("-list")

	assert.Equal(t, exitOK, code)
	for _, id := range []string{"card", "pay", "otp", "otp-tag", "reject", "limit", "guard", "contact"} {
		assert.Contains(t, h.out.String(), id)
	}
	assert.Zero(t, h.opened, "listing opens no browser")
}

func TestRun_APathThatPassesWritesItsEvidence(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte con esa solicitud"`})

	code := h.run("-path", "guard")

	require.Equal(t, exitOK, code, h.err.String())
	assert.Contains(t, h.out.String(), "guard")
	assert.Contains(t, h.out.String(), "PASSED")
	assert.Equal(t, 1, h.sess.fresh, "each path starts a conversation of its own")
	assert.True(t, h.sess.closed)
	report, err := os.ReadFile(filepath.Join(h.dir, "guard", "report.md"))
	require.NoError(t, err)
	assert.Contains(t, string(report), "# guard: PASSED")
	for _, pattern := range []string{"01-ignora-*.snapshot.txt", "01-ignora-*.network.txt", "01-ignora-*.png"} {
		found, err := filepath.Glob(filepath.Join(h.dir, "guard", pattern))
		require.NoError(t, err)
		assert.Len(t, found, 1, pattern)
	}
	assert.FileExists(t, filepath.Join(h.dir, "summary.md"))
}

func TestRun_APathThatFailsIsExit2AndSaysWhy(t *testing.T) {
	h := newHarness(t, &fakeSession{text: "nothing useful"})

	code := h.run("-path", "guard")

	assert.Equal(t, exitFailed, code)
	assert.Contains(t, h.out.String(), "FAILED")
	assert.Contains(t, h.out.String(), "No puedo ayudarte")
}

func TestRun_AllRunsEveryPathThatIsSafeAndEndsAtTheWorst(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})

	code := h.run("-all", "-with-guard")

	assert.Equal(t, exitFailed, code, "the page shows cards of no kind, so most paths fail")
	assert.Equal(t, len(explore.Paths()), h.sess.fresh)
	summary, err := os.ReadFile(filepath.Join(h.dir, "summary.md"))
	require.NoError(t, err)
	for _, p := range explore.Paths() {
		assert.Contains(t, string(summary), "| "+p.ID+" |")
	}
	assert.Contains(t, string(summary), "| guard | PASSED |")
}

func TestRun_StepsGivenOnTheLineAreAPathOfTheirOwn(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "S/500"`})

	code := h.run("Hola", "expect:S/500", "expect-no-card")

	require.Equal(t, exitOK, code, h.err.String())
	assert.FileExists(t, filepath.Join(h.dir, "custom", "report.md"))
}

func TestRun_AnAmountAndAContactAreForPayments(t *testing.T) {
	h := newHarness(t, &fakeSession{cards: []explore.Card{{Kind: "approval", State: "pending"}}})

	code := h.run("-path", "card", "-amount", "35", "-contact", "Maria Torres")

	require.Equal(t, exitOK, code, h.err.String())
	report, err := os.ReadFile(filepath.Join(h.dir, "card", "report.md"))
	require.NoError(t, err)
	assert.Contains(t, string(report), "@pick:Maria Torres")
	assert.Contains(t, string(report), "type:35 soles")
}

func TestRun_UsageMistakes(t *testing.T) {
	tests := map[string][]string{
		"nothing to do":          {},
		"no such path":           {"-path", "nope"},
		"a path and steps":       {"-path", "pay", "Hola"},
		"all and a path":         {"-all", "-path", "pay"},
		"a step that is no step": {"wait:soon"},
		"an amount that is not":  {"-path", "pay", "-amount", "lots"},
		"a negative amount":      {"-path", "pay", "-amount", "-5"},
		"an unknown flag":        {"-nope"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &fakeSession{})

			code := h.run(args...)

			assert.Equal(t, exitUsage, code)
			assert.NotEmpty(t, h.err.String())
			assert.Zero(t, h.opened, "a mistake in the call opens no browser")
		})
	}
}

func TestRun_NoUserToSignInAsIsAUsageMistake(t *testing.T) {
	h := newHarness(t, &fakeSession{})
	h.cfg.Email = ""

	code := h.run("-path", "guard")

	assert.Equal(t, exitUsage, code)
	assert.Contains(t, h.err.String(), "ADMIN_EMAIL")
	assert.Zero(t, h.opened)
}

func TestRun_ASettingsFileThatCannotBeReadIsAUsageMistake(t *testing.T) {
	h := newHarness(t, &fakeSession{})
	h.cfgErr = errors.New("EVAL_BASE_URL: not a URL")

	code := h.run("-path", "guard")

	assert.Equal(t, exitUsage, code)
	assert.Contains(t, h.err.String(), "EVAL_BASE_URL")
}

func TestRun_ABrowserThatCannotBeOpenedIsExit4WithoutTheSecret(t *testing.T) {
	h := newHarness(t, &fakeSession{})
	h.openErr = errors.New("signing in as admin@example.com failed")

	code := h.run("-path", "guard")

	assert.Equal(t, exitInfra, code)
	assert.Contains(t, h.err.String(), "signing in")
	assert.NotContains(t, h.out.String()+h.err.String(), "hunter2-secret")
}

func TestRun_AnInterruptionIsReportedAndStillWritesWhatItHas(t *testing.T) {
	h := newHarness(t, &fakeSession{failType: true})

	code := h.run("-path", "guard")

	assert.Equal(t, exitFailed, code)
	assert.Contains(t, h.out.String(), "the composer is gone")
	assert.True(t, h.sess.closed)
}

func TestRun_NoSecretReachesAFile(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte" textbox "hunter2-secret"`})

	h.run("-path", "guard")

	err := filepath.WalkDir(h.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".png") {
			return err
		}
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.NotContains(t, string(data), "hunter2-secret", path)
		return nil
	})
	require.NoError(t, err)
}

func TestRun_TheDefaultOutputDirectoryIsNamedAfterTheTime(t *testing.T) {
	d := deps{now: func() time.Time { return time.Date(2026, 11, 19, 20, 30, 5, 0, time.UTC) }}

	assert.Equal(t, filepath.Join("temp", "iape-explore", "20261119-203005"), d.defaultOut())
}

func TestRun_HeadlessAndTheTimeToSettleAreHandedToTheBrowser(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})

	require.Equal(t, exitOK, h.run("-path", "guard", "-headless", "-wait", "45s"), h.err.String())
	assert.True(t, h.openedWith.Headless)
	assert.Equal(t, 45*time.Second, h.openedWith.Settle)

	require.Equal(t, exitOK, h.run("-path", "guard"))
	assert.False(t, h.openedWith.Headless, "a window is the default, to watch the run")
}

func TestRun_AllLeavesOutThePathThatBurnsTheAccountUnlessAskedFor(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})

	h.run("-all")

	assert.Equal(t, len(explore.Paths())-1, h.sess.fresh, "guard is not run")
	assert.Contains(t, h.err.String(), "leaves out guard")
	summary, err := os.ReadFile(filepath.Join(h.dir, "summary.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(summary), "| guard |")

	h = newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})
	h.run("-all", "-with-guard")

	assert.Equal(t, len(explore.Paths()), h.sess.fresh, "asked for, it is run")
	assert.NotContains(t, h.err.String(), "leaves out guard")
	assert.Contains(t, h.err.String(), "guard trips the agent's guard", "and it says what it costs")
}

func TestRun_RunningThePathThatBurnsTheAccountSaysWhatItCosts(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})

	require.Equal(t, exitOK, h.run("-path", "guard"))

	assert.Contains(t, h.err.String(), "can get it banned")
}

// Nothing the explorer writes holds a secret, report.md and summary.md included: they carry what was on the page, the steps as they were
// written and the errors, none of which goes through the sink of the snapshots.
func TestRun_TheReportAndTheSummaryHoldNoSecretEither(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk"
	page := "No puedo ayudarte\ntoken " + jwt + "\nBearer abc.def-ghi123456\nthe password is hunter2-secret"
	h := newHarness(t, &fakeSession{text: page})
	h.wrap = func(f *fakeSession) session { return &growingSession{fakeSession: f} } // the page is empty first, so all of it is new

	h.run("-path", "guard")
	h.run("-all")

	var seen int
	err := filepath.WalkDir(h.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(path, "report.md") || strings.HasSuffix(path, "summary.md")) {
			return err
		}
		seen++
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.NotContains(t, string(data), jwt, path)
		assert.NotContains(t, string(data), "abc.def-ghi123456", path)
		assert.NotContains(t, string(data), "hunter2-secret", path)
		return nil
	})
	require.NoError(t, err)
	assert.Positive(t, seen, "the report and the summary were written")
	assert.NotContains(t, h.out.String(), jwt, "nor does the screen")
	assert.NotContains(t, h.err.String(), jwt)
}

// growingSession shows an empty page the first time it is read and the whole page after: what the page shows is then new.
type growingSession struct {
	*fakeSession
	reads int
}

func (g *growingSession) Text(ctx context.Context) (string, error) {
	g.reads++
	if g.reads == 1 {
		return "", nil
	}
	return g.fakeSession.Text(ctx)
}

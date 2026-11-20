package main

import (
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
)

func TestParseOptions_Wait(t *testing.T) {
	tests := map[string]struct {
		value string
		want  string
	}{
		"zero":            {"0", "above zero"},
		"zero seconds":    {"0s", "above zero"},
		"negative":        {"-1s", "above zero"},
		"not a duration":  {"abc", "invalid value"},
		"no unit":         {"5", "invalid value"},
		"an empty string": {"", "invalid value"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseOptions([]string{"-list", "-wait", tc.value})

			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
			assert.ErrorAs(t, err, new(usageError))
		})
	}
	t.Run("a small duration is fine", func(t *testing.T) {
		o, err := parseOptions([]string{"-list", "-wait", "1ms"})

		require.NoError(t, err)
		assert.Equal(t, time.Millisecond, o.settle)
	})
}

func TestParseOptions_Amount(t *testing.T) {
	t.Run("a path that ignores it takes a valid amount without a word", func(t *testing.T) {
		_, err := parseOptions([]string{"-path", "guard", "-amount", "35"})

		require.NoError(t, err)
	})
	t.Run("an amount that is no amount is refused whatever the path", func(t *testing.T) {
		for _, amount := range []string{"NaN", "Inf", "1e3", "+5", " 5", "5.", "0", "٥", "5,5", "1000000"} {
			_, err := parseOptions([]string{"-path", "guard", "-amount", amount})

			require.Error(t, err, amount)
			assert.ErrorContains(t, err, "not an amount of soles")
		}
	})
	t.Run("a plain decimal is accepted", func(t *testing.T) {
		for _, amount := range []string{"5", "20.5", "150.00"} {
			_, err := parseOptions([]string{"-path", "pay", "-amount", amount})

			require.NoError(t, err, amount)
		}
	})
}

func TestParseOptions_Contact(t *testing.T) {
	t.Run("a very long contact is not this command's to refuse", func(t *testing.T) {
		long := strings.Repeat("M", 10_000)

		o, err := parseOptions([]string{"-path", "pay", "-contact", long})

		require.NoError(t, err)
		assert.Equal(t, long, o.contact)
	})
	t.Run("control characters are refused", func(t *testing.T) {
		for _, contact := range []string{"a\nb", "a\tb", "a\x00b", "\x1b[31m", "a\rb", "a\u0085b"} {
			_, err := parseOptions([]string{"-path", "pay", "-contact", contact})

			require.Error(t, err, "%q", contact)
			assert.ErrorContains(t, err, "control character")
		}
	})
	t.Run("unicode, quotes and regex characters are a name like any other", func(t *testing.T) {
		for _, contact := range []string{"José Ñandú", `O'Brien "Jr"`, "a.*b(c", "日本語", "😀"} {
			_, err := parseOptions([]string{"-path", "pay", "-contact", contact})

			require.NoError(t, err, contact)
		}
	})
}

func TestParseOptions_WhatIsChosen(t *testing.T) {
	tests := map[string]struct {
		args []string
		want string
	}{
		"all and list":              {[]string{"-all", "-list"}, "one of"},
		"list and a path":           {[]string{"-list", "-path", "pay"}, "one of"},
		"list and steps":            {[]string{"-list", "Hola"}, "one of"},
		"all and steps":             {[]string{"-all", "Hola"}, "one of"},
		"all, list, path and steps": {[]string{"-all", "-list", "-path", "pay", "Hola"}, "one of"},
		"an empty path":             {[]string{"-path", ""}, "-path"},
		"an empty path and steps":   {[]string{"-path", "", "Hola"}, "-path"},
		"an empty path and all":     {[]string{"-path", "", "-all"}, "-path"},
		"upper case path":           {[]string{"-path", "PAY"}, `no path "PAY"`},
		"mixed case path":           {[]string{"-path", "Otp-Tag"}, "no path"},
		"a path with a space":       {[]string{"-path", "pay "}, "no path"},
		"the flag without a value":  {[]string{"-path"}, "needs an argument"},
		"the same flag twice":       {[]string{"-path", "pay", "-path", "nope"}, "no path"},
		"with-guard alone":          {[]string{"-with-guard"}, "nothing to do"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseOptions(tc.args)

			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
			assert.ErrorAs(t, err, new(usageError))
		})
	}
	t.Run("the last of a repeated flag wins", func(t *testing.T) {
		o, err := parseOptions([]string{"-path", "nope", "-path", "pay"})

		require.NoError(t, err)
		assert.Equal(t, "pay", o.pathID)
	})
	t.Run("two dashes work as one", func(t *testing.T) {
		o, err := parseOptions([]string{"--path=pay", "--headless"})

		require.NoError(t, err)
		assert.Equal(t, "pay", o.pathID)
		assert.True(t, o.headless)
	})
}

func TestParseOptions_AFlagAfterTheStepsIsNotAMessage(t *testing.T) {
	// Go's flag package stops at the first argument that is no flag: without a check, "-headless" would be sent to the agent as text.
	for _, args := range [][]string{
		{"Hola", "-headless"},
		{"Hola", "-wait", "1s"},
		{"Hola", "--headless", "expect:Hola"},
		{"-headless", "Hola", "-out", "x"},
		{"Hola", "-list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := parseOptions(args)

			require.Error(t, err)
			assert.ErrorAs(t, err, new(usageError))
			assert.ErrorContains(t, err, "before the steps")
		})
	}
	t.Run("the error names the flag", func(t *testing.T) {
		_, err := parseOptions([]string{"Hola", "-headless"})

		assert.ErrorContains(t, err, `"-headless"`)
	})
	t.Run("after -- everything is a step, so a message may start with a dash", func(t *testing.T) {
		o, err := parseOptions([]string{"-headless", "--", "-hola", "expect:-hola"})

		require.NoError(t, err)
		assert.Equal(t, []string{"-hola", "expect:-hola"}, o.steps)
		assert.True(t, o.headless)
	})
	t.Run("a dash inside a step, or after a prefix, is only text", func(t *testing.T) {
		o, err := parseOptions([]string{"Hola -headless", "type:-5", "expect:-x", "a-b"})

		require.NoError(t, err)
		assert.Len(t, o.steps, 4)
	})
	t.Run("flags before the steps still work", func(t *testing.T) {
		o, err := parseOptions([]string{"-headless", "-wait", "5s", "Hola", "send"})

		require.NoError(t, err)
		assert.True(t, o.headless)
		assert.Equal(t, []string{"Hola", "send"}, o.steps)
	})
}

func TestRun_AFlagAfterTheStepsIsAUsageErrorThatOpensNoBrowserAndSendsNothing(t *testing.T) {
	h := newHarness(t, &fakeSession{})

	code := h.run("Hola", "-headless")

	assert.Equal(t, exitUsage, code)
	assert.Contains(t, h.err.String(), "before the steps")
	assert.Zero(t, h.opened)
	assert.NoDirExists(t, h.dir)
}

func TestRun_TheExitCodesAreTheOnesTheHelpPromises(t *testing.T) {
	assert.Equal(t, 0, exitOK)
	assert.Equal(t, 1, exitUsage)
	assert.Equal(t, 2, exitFailed)
	assert.Equal(t, 4, exitInfra)
	for _, line := range []string{"0 every path ended where it should", "1 the call or the settings are wrong", "2 a path failed", "4 Chrome could not"} {
		assert.Contains(t, helpText(), line)
	}
}

// freshFails is a session whose Fresh fails.
type freshFails struct{ *fakeSession }

func (freshFails) Fresh(context.Context) error { return errors.New("the start page does not open") }

// cancelsOnSend is a session that is interrupted when a message is sent.
type cancelsOnSend struct {
	*fakeSession
	cancel context.CancelFunc
}

func (c cancelsOnSend) Send(context.Context) error { c.cancel(); return nil }

func newDeps(h *harness) deps {
	return deps{
		stdout: &h.out, stderr: &h.err,
		loadSettings: func(string) (settings, error) { return h.cfg, h.cfgErr },
		open:         func(context.Context, settings, openOptions) (session, error) { h.opened++; return h.sess, h.openErr },
		now:          func() time.Time { return time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC) },
	}
}

func TestRunPlan_APlanThatCannotBeReadIsAFailureOfItsStartAndStartsNoConversation(t *testing.T) {
	sess := &fakeSession{}
	h := newHarness(t, sess)
	d := newDeps(h)

	rep := d.runPlan(t.Context(), sess, plan{id: "broken", steps: []string{"Hola", "wait:soon"}}, filepath.Join(h.dir, "broken"), "pw")

	require.False(t, rep.Passed())
	require.Len(t, rep.Steps, 1)
	assert.Equal(t, "(start)", rep.Steps[0].Raw)
	assert.ErrorContains(t, rep.Failure().Err, "step 2")
	assert.Zero(t, sess.fresh, "nothing is started for a plan that cannot run")
}

func TestRunPlan_AConversationThatCannotBeStartedIsAFailureOfItsStart(t *testing.T) {
	sess := freshFails{&fakeSession{}}
	h := newHarness(t, sess.fakeSession)
	d := newDeps(h)

	rep := d.runPlan(t.Context(), sess, plan{id: "p", steps: []string{"Hola"}}, filepath.Join(h.dir, "p"), "pw")

	require.False(t, rep.Passed())
	assert.ErrorContains(t, rep.Failure().Err, "does not open")
	assert.Equal(t, 1, rep.Failure().N)
}

func TestRunPlan_APlanWithNoStepsIsNotAPass(t *testing.T) {
	for name, steps := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			sess := &fakeSession{}
			h := newHarness(t, sess)
			d := newDeps(h)

			rep := d.runPlan(t.Context(), sess, plan{id: "nothing", steps: steps}, filepath.Join(h.dir, "nothing"), "pw")

			require.False(t, rep.Passed(), "a run that checks nothing proves nothing")
			assert.ErrorContains(t, rep.Failure().Err, "no steps")
			assert.Zero(t, sess.fresh)
		})
	}
}

func TestRunPlans_ThePathsThatCannotStartAreInTheSummaryAndExit2(t *testing.T) {
	sess := freshFails{&fakeSession{}}
	h := newHarness(t, sess.fakeSession)
	d := newDeps(h)

	code := d.runPlans(t.Context(), sess, []plan{{id: "a", steps: []string{"Hola"}}, {id: "b", steps: []string{"wait:soon"}}}, h.dir, "pw")

	assert.Equal(t, exitFailed, code)
	summary, err := os.ReadFile(filepath.Join(h.dir, "summary.md"))
	require.NoError(t, err)
	assert.Contains(t, string(summary), "| a | FAILED |")
	assert.Contains(t, string(summary), "| b | FAILED |")
}

func TestRunPlans_TheSummaryKeepsEachPathOnOneRowWhateverTheReasonSays(t *testing.T) {
	tests := map[string]string{
		"a pipe":            "expect:a|b",
		"two pipes":         "expect:a||b",
		"a backslash pipe":  `expect:a\|b`,
		"a trailing slash":  `expect:a\`,
		"a pipe at the end": "expect:|",
	}
	for name, step := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &fakeSession{})

			code := h.run(step)

			require.Equal(t, exitFailed, code)
			summary, err := os.ReadFile(filepath.Join(h.dir, "summary.md"))
			require.NoError(t, err)
			rows := strings.Split(strings.TrimSpace(string(summary)), "\n")
			require.Len(t, rows, 3, "a header, its rule and one row: %q", summary)
			assert.Equal(t, 5, cellsOf(rows[2]), "%q", rows[2])
		})
	}
}

// multilineError is a session whose composer breaks with an error that spans lines and holds a pipe.
type multilineError struct{ *fakeSession }

func (multilineError) Type(context.Context, string) error {
	return errors.New("first line | pipe\nsecond line\r\nthird")
}

func TestRunPlans_AReasonThatSpansLinesStaysInItsRow(t *testing.T) {
	h := newHarness(t, &fakeSession{})
	sess := multilineError{&fakeSession{}}
	d := newDeps(h)

	code := d.runPlans(t.Context(), sess, []plan{{id: "p", steps: []string{"Hola"}}}, h.dir, "pw")

	require.Equal(t, exitFailed, code)
	summary, err := os.ReadFile(filepath.Join(h.dir, "summary.md"))
	require.NoError(t, err)
	rows := strings.Split(strings.TrimSpace(string(summary)), "\n")
	require.Len(t, rows, 3, "%q", summary)
	assert.Equal(t, 5, cellsOf(rows[2]), "%q", rows[2])
}

// cellsOf counts the cells of a row of a Markdown table: the pipes that are not escaped split it.
func cellsOf(row string) int {
	n := 0
	for i := 0; i < len(row); i++ {
		switch row[i] {
		case '\\':
			i++
		case '|':
			n++
		}
	}
	return n - 1
}

func TestRun_AnInterruptedRunStillWritesItsSummaryAndIsExit2(t *testing.T) {
	t.Run("interrupted while it runs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		base := &fakeSession{}
		h := newHarness(t, base)
		d := newDeps(h)
		d.open = func(context.Context, settings, openOptions) (session, error) {
			return cancelsOnSend{base, cancel}, nil
		}

		code := d.run(ctx, []string{"-out", h.dir, "-all", "-with-guard"})

		assert.Equal(t, exitFailed, code)
		assert.Equal(t, 1, base.fresh, "the paths after the interrupted one are not started")
		summary, err := os.ReadFile(filepath.Join(h.dir, "summary.md"))
		require.NoError(t, err, "what was done is written down")
		assert.Contains(t, string(summary), "| card | FAILED |")
		assert.NotContains(t, string(summary), "| pay |")
		report, err := os.ReadFile(filepath.Join(h.dir, "card", "report.md"))
		require.NoError(t, err)
		assert.Contains(t, string(report), "The run was interrupted.")
		assert.True(t, base.closed, "the browser is closed even though the context is gone")
	})
	t.Run("interrupted before it starts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		h := newHarness(t, &fakeSession{})
		d := newDeps(h)

		code := d.run(ctx, []string{"-out", h.dir, "-path", "guard"})

		assert.Equal(t, exitFailed, code)
		assert.FileExists(t, filepath.Join(h.dir, "summary.md"))
		assert.True(t, h.sess.closed)
	})
}

func TestRun_TheRecordsDirectoryThatCannotBeWritten(t *testing.T) {
	t.Run("a file where the directory should be", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "a-file")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
		h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})
		h.dir = file

		var code int
		require.NotPanics(t, func() { code = h.run("-path", "guard") })

		assert.Equal(t, exitInfra, code, "the evidence cannot be kept, which is no pass")
		assert.Contains(t, h.err.String(), file, "it names what is in the way")
		assert.Contains(t, h.err.String(), "records")
		assert.Zero(t, h.opened, "Chrome is not opened for a run that cannot keep its records")
	})
	t.Run("a file in the middle of the path", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "a-file")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
		h := newHarness(t, &fakeSession{})
		h.dir = filepath.Join(file, "inside", "out")

		code := h.run("-path", "guard")

		assert.Equal(t, exitInfra, code)
		assert.Zero(t, h.opened)
	})
	t.Run("a directory that is read only", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		parent := t.TempDir()
		require.NoError(t, os.Chmod(parent, 0o500))
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})
		h.dir = filepath.Join(parent, "out")

		var code int
		require.NotPanics(t, func() { code = h.run("-path", "guard") })

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.err.String(), "records")
		assert.NotContains(t, h.out.String()+h.err.String(), "hunter2-secret")
	})
	t.Run("the records cannot be written after the run: the pass is not a clean one", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		base := &fakeSession{text: `StaticText "No puedo ayudarte"`}
		h := newHarness(t, base)
		d := newDeps(h)
		parent := t.TempDir()
		out := filepath.Join(parent, "out")
		d.open = func(context.Context, settings, openOptions) (session, error) {
			// the directory was fine when the run started and is not any more
			require.NoError(t, os.RemoveAll(out))
			require.NoError(t, os.Chmod(parent, 0o500))
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
			return base, nil
		}

		code := d.run(t.Context(), []string{"-out", out, "-path", "guard"})

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.err.String(), "iape-explore:")
	})
}

func TestRun_AnOutputDirectoryThatDoesNotExistYetIsMadeWithItsParents(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "No puedo ayudarte"`})
	h.dir = filepath.Join(t.TempDir(), "a", "b", "c")

	code := h.run("-path", "guard")

	require.Equal(t, exitOK, code, h.err.String())
	assert.FileExists(t, filepath.Join(h.dir, "summary.md"))
}

func TestRun_NoSecretAppearsInWhatTheCommandPrintsEither(t *testing.T) {
	h := newHarness(t, &fakeSession{text: `StaticText "hunter2-secret"`})

	h.run("expect:nothing here")

	assert.NotContains(t, h.out.String()+h.err.String(), "hunter2-secret")
}

func TestRun_AnEnvFileNamedByTheFlagIsHandedToTheLoader(t *testing.T) {
	h := newHarness(t, &fakeSession{})
	var asked string
	d := newDeps(h)
	d.loadSettings = func(envFile string) (settings, error) { asked = envFile; return h.cfg, nil }

	d.run(t.Context(), []string{"-out", h.dir, "-env", "other.env", "-list"})
	assert.Empty(t, asked, "listing reads no settings")

	d.run(t.Context(), []string{"-out", h.dir, "-env", "other.env", "-path", "guard"})
	assert.Equal(t, "other.env", asked)
}

func TestPlans(t *testing.T) {
	t.Run("-all leaves out the paths that burn the account", func(t *testing.T) {
		got := options{all: true}.plans()

		for _, p := range got {
			assert.False(t, p.burns, p.id)
		}
		assert.Len(t, got, len(explore.Paths())-1)
	})
	t.Run("-all with the guard has them all, with a made up name for each run", func(t *testing.T) {
		got := options{all: true, withGuard: true}.plans()

		assert.Len(t, got, len(explore.Paths()))
	})
	t.Run("an amount and a contact reach every path of -all", func(t *testing.T) {
		got := options{all: true, amount: "35", contact: "Maria"}.plans()

		for _, p := range got {
			if p.id == "pay" {
				assert.Contains(t, p.steps, "type:35 soles")
				assert.Contains(t, p.steps, "@pick:Maria")
			}
		}
	})
	t.Run("steps of your own are the plan called custom, as written", func(t *testing.T) {
		got := options{steps: []string{"Hola", "send"}}.plans()

		assert.Equal(t, []plan{{id: "custom", steps: []string{"Hola", "send"}}}, got)
	})
}

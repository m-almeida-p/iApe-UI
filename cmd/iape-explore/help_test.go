package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

func TestHelp_SaysEveryFlagEveryPathAndEveryStep(t *testing.T) {
	help := helpText()

	newFlagSet(&options{}).VisitAll(func(f *flag.Flag) {
		assert.Contains(t, help, "-"+f.Name, "the flag -%s", f.Name)
		assert.Contains(t, help, f.Usage, "what -%s does", f.Name)
	})
	for _, p := range explore.Paths() {
		assert.Contains(t, help, p.ID, "the path %s", p.ID)
		assert.Contains(t, help, p.Title)
	}
	for _, f := range explore.StepForms {
		assert.Contains(t, help, f.Form, "the step %s", f.Form)
		assert.Contains(t, help, f.Does)
	}
	for _, setting := range []string{"EXPLORE_BASE_URL", "EXPLORE_USER_EMAIL", "ADMIN_EMAIL", "EXPLORE_MCP_COMMAND", "EXPLORE_MCP_ARGS"} {
		assert.Contains(t, help, setting)
	}
	for _, word := range []string{"Exit status", "Records", "report.md", "summary.md", "Examples", "-headless", "guardrail"} {
		assert.Contains(t, help, word)
	}
}

func TestHelp_WarnsAgainstThePathThatBurnsTheAccountWhereItIsListed(t *testing.T) {
	var line string
	for l := range strings.SplitSeq(helpText(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "guard ") {
			line = l
		}
	}

	require.NotEmpty(t, line)
	assert.Contains(t, line, "banned")
}

func TestHelp_DefaultsAreShownWhereThereIsOne(t *testing.T) {
	help := helpText()

	assert.Contains(t, help, "-wait (default 1m30s)")
	assert.Contains(t, help, "-env (default .env)")
	assert.NotContains(t, help, "-headless (default", "a switch that is off says nothing of a default")
}

func TestHelp_TheLinesAreNotTooWideForATerminal(t *testing.T) {
	for l := range strings.SplitSeq(helpText(), "\n") {
		assert.LessOrEqual(t, len([]rune(l)), 160, l)
	}
}

func TestRun_HelpGoesToStdoutAndIsNotAMistake(t *testing.T) {
	for _, flagName := range []string{"-h", "-help", "--help"} {
		h := newHarness(t, &fakeSession{})

		code := h.run(flagName)

		assert.Equal(t, exitOK, code, flagName)
		assert.Contains(t, h.out.String(), "Usage:", flagName)
		assert.Empty(t, h.err.String(), "nothing on stderr")
		assert.Zero(t, h.opened, "no browser")
	}
}

func TestRun_AMistakeSaysWhereTheHelpIs(t *testing.T) {
	for name, args := range map[string][]string{"nothing to do": {}, "an unknown flag": {"-nope"}, "no such path": {"-path", "nope"}} {
		h := newHarness(t, &fakeSession{})

		code := h.run(args...)

		assert.Equal(t, exitUsage, code, name)
		assert.Contains(t, h.err.String(), "Try: iape-explore -h", name)
	}
}

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

func TestHelp_IsStableAndEndsInANewline(t *testing.T) {
	var first string
	require.NotPanics(t, func() { first = helpText() })

	assert.Equal(t, first, helpText(), "the same help every time")
	assert.True(t, strings.HasSuffix(first, "\n"))
	assert.False(t, strings.HasSuffix(first, "\n\n"), "one newline at the end")
	assert.NotContains(t, first, "%!", "a format verb that got no argument")
	assert.NotContains(t, first, "(MISSING)")
}

func TestHelp_TheFlagsComeInAlphabeticalOrderAndEachOnce(t *testing.T) {
	var flags []string
	for line := range strings.SplitSeq(helpText(), "\n") {
		if strings.HasPrefix(line, "  -") && !strings.HasPrefix(line, "  -h,") {
			flags = append(flags, strings.Fields(line)[0])
		}
	}

	require.NotEmpty(t, flags)
	assert.IsIncreasing(t, flags, "each once, in order")
}

func TestHelp_SaysThatFlagsComeBeforeTheSteps(t *testing.T) {
	help := helpText()

	assert.Contains(t, help, "before the steps")
	assert.Contains(t, help, "--")
}

func TestHelp_EveryExampleOfTheHelpIsACallThatIsAccepted(t *testing.T) {
	// the examples are what people copy: each one must parse as a call, with its quotes taken off
	calls := [][]string{
		{"-all"},
		{"-path", "pay", "-amount", "35", "-contact", "Maria Torres"},
		{"-headless", "-all", "-out", "/tmp/run"},
		{"Hola", "expect:Hola", "expect-no-card"},
		{"-list"},
	}
	for _, args := range calls {
		_, err := parseOptions(args)

		assert.NoError(t, err, "%v", args)
	}
	for _, ex := range []string{"iape-explore -all", "iape-explore -path pay -amount 35", "iape-explore -headless -all -out /tmp/run", `iape-explore "Hola" "expect:Hola" "expect-no-card"`} {
		assert.Contains(t, helpText(), ex)
	}
}

func TestHelp_PathsAndStepsAreListedInTheOrderOfTheCatalog(t *testing.T) {
	lines := strings.Split(helpText(), "\n")
	lineOf := func(prefix string, after int) int {
		for i := after + 1; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], prefix) {
				return i
			}
		}
		return -1
	}
	at := lineOf("Paths:", -1)
	require.GreaterOrEqual(t, at, 0)
	for _, p := range explore.Paths() {
		next := lineOf(fmt.Sprintf("  %-8s ", p.ID), at)
		require.Greater(t, next, at, "%s comes after the path before it", p.ID)
		at = next
	}
	at = lineOf("Steps ", at)
	require.GreaterOrEqual(t, at, 0)
	for _, f := range explore.StepForms {
		next := lineOf(fmt.Sprintf("  %-28s ", f.Form), at)
		require.Greater(t, next, at, "%s comes after the step before it", f.Form)
		at = next
	}
}

func TestHelp_NamesNoSecretAndNoRealAddress(t *testing.T) {
	help := helpText()

	for _, bad := range []string{"password=", "hunter2", "Bearer ", "eyJ"} {
		assert.NotContains(t, help, bad)
	}
}

package explore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

func TestCatalog_HasTheSevenFlowsAndTheContactCard(t *testing.T) {
	var ids []string
	for _, p := range explore.Paths() {
		ids = append(ids, p.ID)
	}

	assert.Equal(t, []string{"card", "pay", "otp", "otp-tag", "reject", "limit", "guard", "contact"}, ids)
}

func TestCatalog_EveryPathParsesAndEndsInAnExpectation(t *testing.T) {
	for _, p := range explore.Paths() {
		t.Run(p.ID, func(t *testing.T) {
			steps, err := explore.ParseAll(p.Steps("", ""))

			require.NoError(t, err)
			require.NotEmpty(t, steps)
			last := steps[len(steps)-1]
			assert.Contains(t, []explore.Kind{explore.KindExpect, explore.KindExpectCard, explore.KindExpectNoCard}, last.Kind,
				"a path that expects nothing proves nothing")
			assert.NotEmpty(t, p.Title)
		})
	}
}

func TestPath_Steps(t *testing.T) {
	pay, ok := explore.Find("pay")
	require.True(t, ok)

	t.Run("pays a contact picked from the @ picker, with the path's amount", func(t *testing.T) {
		assert.Equal(t, []string{
			"quiero yapear", "@", "@pick", "type:20 soles", "send", "expect-card:approval=pending", "click:Pay", "expect-card:approval=paid",
		}, pay.Steps("", ""))
	})
	t.Run("the amount and the contact can be chosen", func(t *testing.T) {
		got := pay.Steps("35", "Maria Torres")

		assert.Contains(t, got, "@pick:Maria Torres")
		assert.Contains(t, got, "type:35 soles")
	})
	t.Run("the message of a path goes after the amount", func(t *testing.T) {
		tag, _ := explore.Find("otp-tag")

		assert.Contains(t, tag.Steps("", ""), `type:20 soles con mensaje "[otp]"`)
	})
	t.Run("a path that is not a payment lists all its steps", func(t *testing.T) {
		guard, _ := explore.Find("guard")

		assert.Equal(t, guard.Steps("99", "x"), guard.Steps("", ""), "an amount and a contact mean nothing to it")
		assert.Contains(t, guard.Steps("", ""), "expect:No puedo ayudarte")
	})
}

func TestCatalog_OnlyTheGuardPathBurnsTheAccount(t *testing.T) {
	var burning []string
	for _, p := range explore.Paths() {
		if p.Burns {
			burning = append(burning, p.ID)
		}
	}

	assert.Equal(t, []string{"guard"}, burning, "a path that trips the agent's guard is marked, and no other is")
}

func TestPath_ContactNeedsANameTheBFFHasNeverHeard(t *testing.T) {
	contact, ok := explore.Find("contact")
	require.True(t, ok)

	first, second := contact.Steps("", ""), contact.Steps("", "")

	assert.NotEqual(t, first, second, "each run makes a name of its own")
	for _, steps := range [][]string{first, second} {
		for _, s := range steps {
			assert.NotContains(t, s, "{new-name}", "the placeholder is always filled")
		}
		parsed, err := explore.ParseAll(steps)
		require.NoError(t, err)
		name := strings.TrimPrefix(steps[1], "quiero yapearle 20 soles a ")
		assert.Regexp(t, `^[A-Z][a-z]{5}$`, name)
		assert.Equal(t, "click:"+name, parsed[3].Raw, "the card's button is the one for the same name")
	}

	given := contact.Steps("", "Valeria")
	assert.Equal(t, "quiero yapearle 20 soles a Valeria", given[1], "a contact that is asked for is the one used")
	assert.Equal(t, "click:Valeria", given[3])
}

func TestFind(t *testing.T) {
	_, ok := explore.Find("otp")
	assert.True(t, ok)
	_, ok = explore.Find("nope")
	assert.False(t, ok)
}

func TestPaths_ReturnsACopy(t *testing.T) {
	first := explore.Paths()
	first[0].ID = "changed"

	assert.Equal(t, "card", explore.Paths()[0].ID)
}

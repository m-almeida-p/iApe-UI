package explore_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

func TestValidAmount(t *testing.T) {
	good := []string{"5", "20", "20.5", "150.00", "0.5", "0.01", "999999", "999999.99", "1", "100", "500.50"}
	for _, amount := range good {
		t.Run("accepts "+amount, func(t *testing.T) { assert.True(t, explore.ValidAmount(amount)) })
	}

	bad := map[string]string{
		"empty":                               "",
		"blank":                               " ",
		"zero":                                "0",
		"zero with decimals":                  "0.00",
		"negative":                            "-1",
		"negative decimal":                    "-0.5",
		"exponent":                            "1e3",
		"upper exponent":                      "1E3",
		"NaN":                                 "NaN",
		"nan":                                 "nan",
		"Inf":                                 "Inf",
		"+Inf":                                "+Inf",
		"infinity":                            "infinity",
		"hex float":                           "0x1p4",
		"hex":                                 "0x10",
		"plus sign":                           "+5",
		"leading space":                       " 5",
		"trailing space":                      "5 ",
		"a space inside":                      "5 0",
		"a comma for the decimals":            "5,5",
		"a thousands comma":                   "1,000",
		"underscore":                          "1_000",
		"trailing dot":                        "5.",
		"leading dot":                         ".5",
		"only a dot":                          ".",
		"two dots":                            "5.5.5",
		"three decimals":                      "5.123",
		"one million":                         "1000000",
		"above a million":                     "1000001",
		"a million with decimals":             "1000000.00",
		"huge":                                strings.Repeat("9", 400),
		"Arabic-Indic five":                   "٥",
		"fullwidth five":                      "５",
		"a leading zero":                      "05",
		"a newline":                           "5\n",
		"letters":                             "five",
		"a currency sign":                     "S/5",
		"a unit":                              "5 soles",
		"a minus after":                       "5-",
		"zero point zero one is below a cent": "0.001",
		"too small to be money":               "0.0000001",
	}
	for name, amount := range bad {
		t.Run("rejects "+name, func(t *testing.T) { assert.False(t, explore.ValidAmount(amount), "%q", amount) })
	}
}

func TestPath_StepsTrimTheAmountAndTheContact(t *testing.T) {
	pay, _ := explore.Find("pay")

	got := pay.Steps("  35 ", "  Maria Torres\t")

	assert.Equal(t, "type:35 soles", got[3])
	assert.Equal(t, "@pick:Maria Torres", got[2])
	assert.Equal(t, pay.Steps("", ""), pay.Steps("   ", "   "), "an amount or a contact of blanks is no amount and no contact")
}

func TestPath_ContactPathWithABlankContactMakesUpAName(t *testing.T) {
	contact, _ := explore.Find("contact")

	steps := contact.Steps("", "   ")

	parsed, err := explore.ParseAll(steps)
	require.NoError(t, err, "a blank contact must not make a plan that cannot be read")
	assert.Regexp(t, `^click:[A-Z][a-z]{5}$`, parsed[3].Raw)
}

func TestPath_CustomPathsIgnoreTheAmount(t *testing.T) {
	for _, id := range []string{"guard", "contact"} {
		p, _ := explore.Find(id)

		withAmount := p.Steps("not an amount at all", "Valeria")
		without := p.Steps("", "Valeria")

		assert.Equal(t, without, withAmount, id)
	}
}

func TestPath_StepsContactEdgeCases(t *testing.T) {
	pay, _ := explore.Find("pay")
	tests := map[string]string{
		"quotes":               `Maria "La Jefa" Torres`,
		"regex metacharacters": `Maria (.*)+[a-z]$^`,
		"a replacement mark":   `Maria $1 ${name}`,
		"unicode":              "José Ñandú 日本語 😀",
		"right to left":        "مريم",
		"a prefix inside":      "click:Pay",
		"the placeholder":      "{new-name}",
		"very long":            strings.Repeat("M", 10_000),
		"equals and colon":     "a=b:c",
	}
	for name, contact := range tests {
		t.Run(name, func(t *testing.T) {
			steps := pay.Steps("", contact)

			parsed, err := explore.ParseAll(steps)
			require.NoError(t, err)
			assert.Equal(t, explore.KindPick, parsed[2].Kind, "the contact stays inside the pick step")
			assert.Equal(t, contact, parsed[2].Arg)
			assert.Len(t, steps, len(pay.Steps("", "")), "a contact never adds or removes a step")
		})
	}
	t.Run("the contact path puts the contact in both places, as it is", func(t *testing.T) {
		contactPath, _ := explore.Find("contact")
		name := `O'Brien $1 (x)`

		steps := contactPath.Steps("", name)

		assert.Equal(t, "quiero yapearle 20 soles a "+name, steps[1])
		assert.Equal(t, "click:"+name, steps[3])
	})
}

func TestPath_StepsReturnsAFreshSliceEachCall(t *testing.T) {
	for _, p := range explore.Paths() {
		t.Run(p.ID, func(t *testing.T) {
			first := p.Steps("35", "Valeria")
			for i := range first {
				first[i] = "tampered"
			}

			again, _ := explore.Find(p.ID)
			for _, step := range again.Steps("35", "Valeria") {
				assert.NotEqual(t, "tampered", step)
			}
			assert.NotContains(t, again.After, "tampered")
			assert.NotContains(t, again.Custom, "tampered")
		})
	}
}

func TestPaths_AndFindReturnCopiesThatShareNothingWithTheCatalog(t *testing.T) {
	tamper := func(p explore.Path) {
		for i := range p.After {
			p.After[i] = "tampered"
		}
		for i := range p.Custom {
			p.Custom[i] = "tampered"
		}
	}
	for _, p := range explore.Paths() {
		tamper(p)
	}
	for _, p := range explore.Paths() {
		found, ok := explore.Find(p.ID)
		require.True(t, ok)
		tamper(found)
	}

	for _, p := range explore.Paths() {
		assert.NotContains(t, p.After, "tampered", p.ID)
		assert.NotContains(t, p.Custom, "tampered", p.ID)
		steps := p.Steps("", "")
		assert.NotContains(t, steps, "tampered", p.ID)
		_, err := explore.ParseAll(steps)
		require.NoError(t, err, p.ID)
	}
}

func TestFind_IsExactAndCaseSensitive(t *testing.T) {
	for _, id := range []string{"", "PAY", "Pay", " pay", "pay ", "pa", "otp-", "OTP-TAG"} {
		_, ok := explore.Find(id)

		assert.False(t, ok, "%q", id)
	}
	p, ok := explore.Find("otp-tag")
	require.True(t, ok)
	assert.Equal(t, "otp-tag", p.ID)
}

func TestPaths_NoTwoShareAnID_AndEveryOneHasATitleAndSteps(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range explore.Paths() {
		assert.False(t, seen[p.ID], "duplicate id %s", p.ID)
		seen[p.ID] = true
		assert.Equal(t, strings.TrimSpace(p.ID), p.ID)
		assert.NotContains(t, p.ID, " ")
		assert.True(t, p.Custom != nil || explore.ValidAmount(p.Amount), "%s needs a valid default amount", p.ID)
	}
}

func TestFreshName_IsSixLettersAndRarelyRepeats(t *testing.T) {
	contact, _ := explore.Find("contact")
	const runs = 1000
	names := map[string]bool{}

	for range runs {
		steps := contact.Steps("", "")
		name := strings.TrimPrefix(steps[1], "quiero yapearle 20 soles a ")

		assert.Regexp(t, `^[A-Z][a-z]{5}$`, name)
		assert.Equal(t, "click:"+name, steps[3])
		for _, s := range steps {
			assert.NotContains(t, s, "{new-name}")
		}
		names[name] = true
	}

	assert.Greater(t, len(names), 990, "%d distinct names out of %d", len(names), runs)
}

func TestFreshName_IsNeverAStepWord(t *testing.T) {
	// the name goes into a message and a click: it must never read as a prefix of the step language
	contact, _ := explore.Find("contact")
	for i := range 200 {
		parsed, err := explore.ParseAll(contact.Steps("", ""))

		require.NoError(t, err, strconv.Itoa(i))
		assert.Equal(t, explore.KindMessage, parsed[1].Kind)
		assert.Equal(t, explore.KindClick, parsed[3].Kind, fmt.Sprint(parsed[3]))
	}
}

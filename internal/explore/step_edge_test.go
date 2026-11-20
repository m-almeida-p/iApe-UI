package explore_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

func TestParse_OTPCodeIsASCIIDigitsOnly(t *testing.T) {
	tests := map[string]string{
		"Arabic-Indic digits":                   "otp:١٢٣٤٥٦",
		"four Arabic-Indic digits, eight bytes": "otp:١٢٣٤",
		"two Arabic-Indic digits, four bytes":   "otp:١٢",
		"three fullwidth digits":                "otp:１２３",
		"fullwidth digits":                      "otp:１２３４",
		"Arabic-Indic digits and ASCII":         "otp:12٣٤",
		"Devanagari digits":                     "otp:१२३४५६",
		"superscript digits":                    "otp:¹²³⁴",
		"a sign":                                "otp:+12345",
		"a minus":                               "otp:-12345",
		"a space inside":                        "otp:123 456",
		"a dot":                                 "otp:123.45",
		"a newline inside":                      "otp:123\n456",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := explore.Parse(raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "digits")
		})
	}
}

func TestParse_OTPCodeLengthLimits(t *testing.T) {
	tests := map[string]bool{
		"otp:0000":      true,
		"otp:12345678":  true,
		"otp: 123456 ":  true,
		"otp:000123":    true,
		"otp:123":       false,
		"otp:123456789": false,
	}
	for raw, ok := range tests {
		t.Run(raw, func(t *testing.T) {
			s, err := explore.Parse(raw)

			if !ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "otp:")), s.Arg, "the code keeps its zeros and loses its spaces")
		})
	}
}

func TestParse_Wait(t *testing.T) {
	good := map[string]time.Duration{
		"wait:1.5s":       1500 * time.Millisecond,
		"wait:5m":         5 * time.Minute,
		"wait:0.0001s":    100 * time.Microsecond,
		"wait: 2s ":       2 * time.Second,
		"wait:300ms":      300 * time.Millisecond,
		"wait:4m59s":      4*time.Minute + 59*time.Second,
		"wait:+3s":        3 * time.Second,
		"wait:1m30s":      90 * time.Second,
		"wait:299999.9ms": 299999900 * time.Microsecond,
	}
	for raw, want := range good {
		t.Run(raw, func(t *testing.T) {
			s, err := explore.Parse(raw)

			require.NoError(t, err)
			assert.Equal(t, explore.KindWait, s.Kind)
			assert.Equal(t, want, s.Wait)
		})
	}

	bad := map[string]string{
		"wait:5m1s":  "between",
		"wait:":      "duration",
		"wait:5":     "duration",
		"wait:NaN":   "duration",
		"wait:1e3s":  "duration",
		"wait:0":     "between",
		"wait:-1ns":  "between",
		"wait:1000h": "between",
		"wait:5s 5s": "duration",
		"wait:5 s":   "duration",
	}
	for raw, why := range bad {
		t.Run(raw, func(t *testing.T) {
			_, err := explore.Parse(raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), why)
		})
	}
}

func TestParse_PrefixesAreCaseSensitiveSoAnythingElseIsAMessage(t *testing.T) {
	for _, raw := range []string{
		"WAIT:5s", "Send", "SEND", "Click:Pay", "OTP:123456", "Expect:x", "EXPECT-NO-CARD", "Type:x", "@PICK", "@Pick:Juan",
		"expect-card :approval=paid", "wait :5s", "wait 5s", "click Pay", "otp 123456",
	} {
		t.Run(raw, func(t *testing.T) {
			s, err := explore.Parse(raw)

			require.NoError(t, err)
			assert.Equal(t, explore.KindMessage, s.Kind)
			assert.Equal(t, raw, s.Arg)
		})
	}
}

func TestParse_AMessageThatMerelyStartsLikeAStepIsAMessage(t *testing.T) {
	for _, raw := range []string{
		"send it now", "sender", "send:", "send:now", "expect-no-cards", "expect-no-card please", "@pickup", "@picked:Juan", "@@",
		"@juan", "type", "click", "wait", "otp", "expect", "expect-card", "expect-no-card:x",
	} {
		t.Run(raw, func(t *testing.T) {
			s, err := explore.Parse(raw)

			require.NoError(t, err)
			assert.Equal(t, explore.KindMessage, s.Kind, "%q is a message, not a step", raw)
			assert.Equal(t, strings.TrimSpace(raw), s.Arg)
		})
	}
}

func TestParse_BareWordsWithTrailingSpacesAreStillTheStep(t *testing.T) {
	tests := map[string]explore.Kind{
		"send ":             explore.KindSend,
		"  send\t":          explore.KindSend,
		"@ ":                explore.KindType,
		"expect-no-card  ":  explore.KindExpectNoCard,
		" send ":            explore.KindSend,
		"\tclick:Pay\n":     explore.KindClick,
		"@pick:":            explore.KindPick,
		"@pick:   ":         explore.KindPick,
		"@pick:\tJuan\n":    explore.KindPick,
		"expect-no-card\n ": explore.KindExpectNoCard,
	}
	for raw, kind := range tests {
		t.Run(raw, func(t *testing.T) {
			s, err := explore.Parse(raw)

			require.NoError(t, err)
			assert.Equal(t, kind, s.Kind)
			assert.Equal(t, raw, s.Raw, "Raw is the step as it was written")
		})
	}
}

func TestParse_PickWithoutANameChoosesTheFirst(t *testing.T) {
	for _, raw := range []string{"@pick", "@pick:", "@pick:   ", "@pick:\t"} {
		t.Run(raw, func(t *testing.T) {
			s, err := explore.Parse(raw)

			require.NoError(t, err)
			assert.Equal(t, explore.KindPick, s.Kind)
			assert.Empty(t, s.Arg)
		})
	}
}

func TestParse_ClickIfIsNotShadowedByClick(t *testing.T) {
	s, err := explore.Parse("click?:Pay")
	require.NoError(t, err)
	assert.Equal(t, explore.KindClickIf, s.Kind)
	assert.Equal(t, "Pay", s.Arg, "the question mark is not part of the name")

	s, err = explore.Parse("click:?Pay")
	require.NoError(t, err)
	assert.Equal(t, explore.KindClick, s.Kind)
	assert.Equal(t, "?Pay", s.Arg, "after the colon a question mark is text")

	s, err = explore.Parse("click?:click:Pay")
	require.NoError(t, err)
	assert.Equal(t, explore.KindClickIf, s.Kind)
	assert.Equal(t, "click:Pay", s.Arg)

	_, err = explore.Parse("click?")
	require.NoError(t, err, "without the colon it is a message")
}

func TestParse_TextCanHoldAnythingAPersonTypes(t *testing.T) {
	long := strings.Repeat("hola ", 20_000) // 100k characters
	tests := map[string]string{
		"a very long message":     long,
		"emoji":                   "quiero yapear 😀🎉 a mamá",
		"right to left":           "مرحبا بالعالم",
		"Chinese":                 "你好,世界",
		"a tab inside":            "hola\tmundo",
		"a newline inside":        "hola\nmundo",
		"quotes and brackets":     `con mensaje "[otp]" y 'x' <b>`,
		"a pipe":                  "a | b",
		"the colon of a URL":      "http://example.com:8300/x",
		"another prefix inside":   "dime click:Pay",
		"an equals inside":        "a=b=c",
		"zero-width joiner emoji": "👨‍👩‍👧",
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := explore.Parse(text)

			require.NoError(t, err)
			assert.Equal(t, explore.KindMessage, s.Kind)
			assert.Equal(t, strings.TrimSpace(text), s.Arg)
			assert.Equal(t, text, s.Raw)
		})
	}
	t.Run("type: takes the same text", func(t *testing.T) {
		s, err := explore.Parse("type:" + long)

		require.NoError(t, err)
		assert.Equal(t, explore.KindType, s.Kind)
		assert.Len(t, s.Arg, len(strings.TrimSpace(long)))
	})
	t.Run("expect: with a tab inside", func(t *testing.T) {
		s, err := explore.Parse("expect:a\tb")

		require.NoError(t, err)
		assert.Equal(t, "a\tb", s.Arg)
	})
}

func TestParse_TextAfterAPrefixThatIsOnlyWhitespaceIsNothing(t *testing.T) {
	for _, raw := range []string{"type:\t", "type:\n", "type: ", "click:\t", "click?:\n", "expect:\t ", "expect:　"} {
		t.Run(raw, func(t *testing.T) {
			_, err := explore.Parse(raw)

			require.Error(t, err)
		})
	}
}

func TestParse_ExpectCard(t *testing.T) {
	t.Run("spaces around the kind and the state do no harm", func(t *testing.T) {
		for _, raw := range []string{"expect-card: approval = paid", "expect-card:approval =paid", "expect-card:\tapproval=paid\t", "expect-card:approval= paid"} {
			s, err := explore.Parse(raw)

			require.NoError(t, err, raw)
			assert.Equal(t, explore.Card{Kind: "approval", State: "paid"}, s.Card, raw)
		}
	})
	t.Run("a second equals sign is part of the state, which no card has", func(t *testing.T) {
		_, err := explore.Parse("expect-card:approval=paid=extra")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "state")
	})
	t.Run("kinds and states are written in lower case", func(t *testing.T) {
		for _, raw := range []string{"expect-card:Approval=paid", "expect-card:approval=PAID", "expect-card:approval=otp-required"} {
			_, err := explore.Parse(raw)

			require.Error(t, err, raw)
		}
	})
	t.Run("an empty kind or state, or no equals sign", func(t *testing.T) {
		for _, raw := range []string{"expect-card:", "expect-card:=", "expect-card:=paid", "expect-card:approval", "expect-card: = "} {
			_, err := explore.Parse(raw)

			require.Error(t, err, raw)
			assert.Contains(t, err.Error(), "kind=state", raw)
		}
	})
	t.Run("the list of kinds in the error is the same every time", func(t *testing.T) {
		_, first := explore.Parse("expect-card:bill=paid")
		_, second := explore.Parse("expect-card:bill=paid")

		require.Error(t, first)
		assert.Equal(t, first.Error(), second.Error())
		assert.Contains(t, first.Error(), "approval, contact")
	})
	t.Run("expect-no-card takes no argument", func(t *testing.T) {
		s, err := explore.Parse("expect-no-card:")

		require.NoError(t, err)
		assert.Equal(t, explore.KindMessage, s.Kind, "with anything after it, it is no longer the bare step")
	})
}

func TestCard_StringIsKindEqualsState(t *testing.T) {
	assert.Equal(t, "approval=paid", explore.Card{Kind: "approval", State: "paid"}.String())
	assert.Equal(t, "=", explore.Card{}.String())
}

func TestParse_RawIsKeptAsWrittenAndTheErrorsOfEmptyStepsAreClear(t *testing.T) {
	for _, raw := range []string{"", " ", "\t\n", " ", "　"} {
		_, err := explore.Parse(raw)

		require.Error(t, err, "%q", raw)
		assert.Contains(t, err.Error(), "empty")
	}
}

func TestParseAll_EmptyAndNil(t *testing.T) {
	for name, in := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			got, err := explore.ParseAll(in)

			require.NoError(t, err)
			assert.NotNil(t, got, "an empty list of steps is an empty list, not nil")
			assert.Empty(t, got)
		})
	}
}

func TestParseAll_NamesTheFirstStepThatIsWrongAndKeepsOrder(t *testing.T) {
	got, err := explore.ParseAll([]string{"a", "b", "otp:12", "wait:soon"})

	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "step 3")
	assert.NotContains(t, err.Error(), "step 4", "only the first is named")

	ok, err := explore.ParseAll([]string{"one", "two", "send"})
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two", ""}, []string{ok[0].Arg, ok[1].Arg, ok[2].Arg})
}

func TestParseAll_AnEmptyStepInTheMiddleIsNamed(t *testing.T) {
	_, err := explore.ParseAll([]string{"Hola", "", "send"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "step 2")
	assert.Contains(t, err.Error(), "empty")
}

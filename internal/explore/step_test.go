package explore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

func TestParse(t *testing.T) {
	tests := []struct {
		raw  string
		want explore.Step
	}{
		{"quiero yapear", explore.Step{Kind: explore.KindMessage, Arg: "quiero yapear"}},
		{"  Hola  ", explore.Step{Kind: explore.KindMessage, Arg: "Hola"}},
		{"@", explore.Step{Kind: explore.KindType, Arg: "@"}},
		{"@pick", explore.Step{Kind: explore.KindPick}},
		{"@pick:Juan Perez", explore.Step{Kind: explore.KindPick, Arg: "Juan Perez"}},
		{"type:20 soles", explore.Step{Kind: explore.KindType, Arg: "20 soles"}},
		{"type: 20 soles", explore.Step{Kind: explore.KindType, Arg: "20 soles"}},
		{`type:20 soles con mensaje "[otp]"`, explore.Step{Kind: explore.KindType, Arg: `20 soles con mensaje "[otp]"`}},
		{"send", explore.Step{Kind: explore.KindSend}},
		{"click:Pay", explore.Step{Kind: explore.KindClick, Arg: "Pay"}},
		{"click?:Pay", explore.Step{Kind: explore.KindClickIf, Arg: "Pay"}},
		{"wait:8s", explore.Step{Kind: explore.KindWait, Wait: 8 * time.Second}},
		{"otp:123456", explore.Step{Kind: explore.KindOTP, Arg: "123456"}},
		{"expect:S/500", explore.Step{Kind: explore.KindExpect, Arg: "S/500"}},
		{"expect-card:approval=paid", explore.Step{Kind: explore.KindExpectCard, Card: explore.Card{Kind: "approval", State: "paid"}}},
		{"expect-card:contact=confirmed", explore.Step{Kind: explore.KindExpectCard, Card: explore.Card{Kind: "contact", State: "confirmed"}}},
		{"expect-no-card", explore.Step{Kind: explore.KindExpectNoCard}},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := explore.Parse(tc.raw)

			require.NoError(t, err)
			tc.want.Raw = tc.raw
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParse_RefusesWhatCannotBeRun(t *testing.T) {
	tests := map[string]string{
		"":                               "empty",
		"   ":                            "empty",
		"type:":                          "nothing to type",
		"click:":                         "nothing to click",
		"click?:  ":                      "nothing to click",
		"wait:soon":                      "duration",
		"wait:0s":                        "between",
		"wait:-3s":                       "between",
		"wait:10m":                       "between",
		"otp:":                           "digits",
		"otp:12ab56":                     "digits",
		"otp:123":                        "digits",
		"otp:123456789":                  "digits",
		"expect:":                        "nothing to look for",
		"expect-card:paid":               "kind=state",
		"expect-card:approval=":          "kind=state",
		"expect-card:bill=paid":          "kind",
		"expect-card:approval=lost":      "state",
		"expect-card:contact=paid":       "state",
		"expect-card:approval=confirmed": "state",
	}
	for raw, why := range tests {
		t.Run(raw, func(t *testing.T) {
			_, err := explore.Parse(raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), why)
		})
	}
}

func TestParseAll_NamesTheStepThatIsWrong(t *testing.T) {
	_, err := explore.ParseAll([]string{"quiero yapear", "wait:soon", "send"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "step 2")
	assert.Contains(t, err.Error(), `"wait:soon"`)
}

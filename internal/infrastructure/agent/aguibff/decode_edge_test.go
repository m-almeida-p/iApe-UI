package aguibff_test

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
)

// ev is one event of a stream, with the blank line that ends it.
func ev(json string) string { return "data: " + json + "\n\n" }

const (
	runStarted  = `{"type":"RUN_STARTED","runId":"r1"}`
	runFinished = `{"type":"RUN_FINISHED"}`
	msgStart    = `{"type":"TEXT_MESSAGE_START","messageId":"m"}`
	msgEnd      = `{"type":"TEXT_MESSAGE_END","messageId":"m"}`
)

func content(delta string) string {
	return `{"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":` + quoteJSON(delta) + `}`
}

func quoteJSON(s string) string { return strconv.Quote(s) }

// reply is a whole run that says text.
func reply(text string) string {
	return ev(runStarted) + ev(msgStart) + ev(content(text)) + ev(msgEnd) + ev(runFinished)
}

func decode(sse string) ([]domain.AgentEvent, error) {
	return collect(aguibff.Decode(strings.NewReader(sse)))
}

func cardsOf(events []domain.AgentEvent) []domain.Interaction {
	var cards []domain.Interaction
	for _, e := range events {
		if e.Kind == domain.AgentInteraction {
			cards = append(cards, e.Interaction)
		}
	}
	return cards
}

func TestDecode_TheFraming(t *testing.T) {
	want := []string{"delta", "final:hola"}
	tests := []struct {
		name string
		sse  string
	}{
		{"CRLF line endings", strings.ReplaceAll(reply("hola"), "\n", "\r\n")},
		{"a UTF-8 byte order mark at the start", "\xEF\xBB\xBF" + reply("hola")},
		{"data: with no space after the colon", strings.ReplaceAll(reply("hola"), "data: ", "data:")},
		{"comments and keep-alives between events", ": keep-alive\n\n" + strings.ReplaceAll(reply("hola"), "\n\n", "\n\n: keep-alive\n\n")},
		{"the event, id and retry fields are ignored", "retry: 5\n" + strings.ReplaceAll(reply("hola"), "data: ", "event: x\nid: 1\ndata: ")},
		{"blank lines in a row", "\n\n\n" + strings.ReplaceAll(reply("hola"), "\n\n", "\n\n\n\n")},
		{"the last event without its blank line", strings.TrimSuffix(reply("hola"), "\n\n")},
		{"the last event without any line break", strings.TrimSuffix(reply("hola"), "\n\n")[:len(reply("hola"))-2]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decode(tc.sse)

			require.NoError(t, err)
			assert.Equal(t, want, shape(events))
		})
	}
}

func TestDecode_AnEventMayTakeSeveralDataLinesJoinedByLineBreaks(t *testing.T) {
	sse := "data: {\"type\":\"TEXT_MESSAGE_START\",\n" +
		"data: \"messageId\":\"m\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"delta\":\n" +
		"data: \"hola\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_END\"}\n\n" +
		ev(runFinished)

	events, err := decode(sse)

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:hola"}, shape(events), "the SSE spec joins the data lines of one event with a line feed")
}

func TestDecode_AnEventSpreadOverTooManyDataLinesIsRefused(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"

	_, err := decode(strings.Repeat(line, 6) + "\n")

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "too long", "many short lines are as big as one long line")
}

func TestDecode_NothingToDecode(t *testing.T) {
	tests := []struct {
		name string
		sse  string
	}{
		{"an empty stream", ""},
		{"only comments", ": hi\n\n: there\n\n"},
		{"only blank lines", "\n\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decode(tc.sse)

			require.ErrorIs(t, err, domain.ErrAgentUnavailable, "no answer at all is an agent that did not answer")
			assert.NotErrorIs(t, err, domain.ErrIncompleteStream)
			assert.Empty(t, events)
		})
	}
}

func TestDecode_ANonJSONDataIsAnErrorNotAPanic(t *testing.T) {
	for _, data := range []string{"[DONE]", "hello", "{", `{"type":"RUN_STARTED"`, `{"type":5}`, "5", `"text"`, "[1,2]", `{"type":"CUSTOM","type":"CUSTOM"}`} {
		t.Run(data, func(t *testing.T) {
			_, err := decode(ev(runStarted) + ev(data) + ev(runFinished))

			require.ErrorIs(t, err, domain.ErrAgentUnavailable)
			assert.Contains(t, err.Error(), "malformed")
		})
	}
}

func TestDecode_JSONThatIsNotAnEventIsLeftAlone(t *testing.T) {
	events, err := decode(ev(runStarted) + ev("null") + ev("{}") + ev(runFinished))

	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestDecode_UnknownTypesAndUnknownFieldsAreIgnored(t *testing.T) {
	sse := ev(runStarted) +
		ev(`{"type":"STEP_STARTED","stepName":"x","extra":{"a":[1,2,{"b":null}]}}`) +
		ev(`{"type":"STATE_SNAPSHOT","snapshot":{"balance":10}}`) +
		ev(`{"type":"MESSAGES_SNAPSHOT","messages":[{"id":"1","role":"user","content":"hi"}]}`) +
		ev(`{"type":"RAW","event":{"delta":7},"source":"x"}`) +
		ev(`{"type":"TEXT_MESSAGE_START","messageId":"m","brand":"new","timestamp":1}`) +
		ev(`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"hola","future":true}`) +
		ev(msgEnd) + ev(runFinished)

	events, err := decode(sse)

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:hola"}, shape(events))
}

func TestDecode_AnEventTheDecoderDoesNotReadCannotBreakTheStreamByItsShape(t *testing.T) {
	// STATE_DELTA is an AG-UI event whose delta is a JSON Patch: a list, where a text delta is a string.
	sse := ev(runStarted) +
		ev(`{"type":"STATE_DELTA","delta":[{"op":"replace","path":"/a","value":1}]}`) +
		ev(`{"type":"TOOL_CALL_RESULT","toolCallId":"c","content":{"not":"a string"}}`) +
		ev(`{"type":"STEP_FINISHED","message":{"a":1},"runId":7,"name":3}`) +
		ev(msgStart) + ev(content("hola")) + ev(msgEnd) + ev(runFinished)

	events, err := decode(sse)

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:hola"}, shape(events), "only the events the Web UI works with are read")
}

func TestDecode_TheTextOfAMessage(t *testing.T) {
	tests := []struct {
		name string
		sse  string
		want []string
	}{
		{
			"content before the message starts opens it",
			ev(runStarted) + ev(content("a")) + ev(msgEnd) + ev(runFinished),
			[]string{"delta", "final:a"},
		},
		{
			"a second start closes the first message with its own final: nothing is lost",
			ev(runStarted) + ev(msgStart) + ev(content("uno")) + ev(msgStart) + ev(content("dos")) + ev(msgEnd) + ev(runFinished),
			[]string{"delta", "final:uno", "delta", "final:dos"},
		},
		{
			"an end without a start says nothing",
			ev(runStarted) + ev(msgEnd) + ev(runFinished),
			nil,
		},
		{
			"two ends close one message once",
			ev(runStarted) + ev(msgStart) + ev(content("a")) + ev(msgEnd) + ev(msgEnd) + ev(runFinished),
			[]string{"delta", "final:a"},
		},
		{
			"text after RUN_FINISHED is still delivered",
			ev(runStarted) + ev(runFinished) + ev(msgStart) + ev(content("tarde")) + ev(msgEnd),
			[]string{"delta", "final:tarde"},
		},
		{
			"a message with no text has an empty final and no delta",
			ev(runStarted) + ev(msgStart) + ev(msgEnd) + ev(runFinished),
			[]string{"final:"},
		},
		{
			"an empty delta is not delivered but the message still closes",
			ev(runStarted) + ev(msgStart) + ev(content("")) + ev(content("x")) + ev(msgEnd) + ev(runFinished),
			[]string{"delta", "final:x"},
		},
		{
			"a message the stream never closed is closed at the end",
			ev(runStarted) + ev(msgStart) + ev(content("abierto")) + ev(runFinished),
			[]string{"delta", "final:abierto"},
		},
		{
			"HTML, emoji and escapes are kept verbatim",
			reply("<b>hola</b> 👋 \"q\" \\ \n\t ñ  "),
			[]string{"delta", "final:<b>hola</b> 👋 \"q\" \\ \n\t ñ  "},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decode(tc.sse)

			require.NoError(t, err)
			assert.Equal(t, tc.want, shape(events))
		})
	}
}

func TestDecode_InvalidUTF8InADeltaIsAnErrorNotAPanic(t *testing.T) {
	sse := ev(runStarted) + ev(msgStart) + "data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"delta\":\"ho\xffla\"}\n\n" + ev(runFinished)

	events, err := decode(sse)

	require.ErrorIs(t, err, domain.ErrAgentUnavailable, "a text that is not UTF-8 is not text: JSON cannot carry it")
	assert.Empty(t, events)
}

func TestDecode_BigDeltas(t *testing.T) {
	t.Run("3 MB fits under the line limit and arrives whole", func(t *testing.T) {
		big := strings.Repeat("x", 3<<20)

		events, err := decode(reply(big))

		require.NoError(t, err)
		require.Len(t, events, 2)
		assert.Equal(t, big, events[1].Text)
	})
	t.Run("5 MB is over the line limit", func(t *testing.T) {
		_, err := decode(reply(strings.Repeat("x", 5<<20)))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Contains(t, err.Error(), "too long")
	})
	t.Run("a long line after some text still ends the stream with the line error", func(t *testing.T) {
		events, err := decode(ev(runStarted) + ev(msgStart) + ev(content("a")) + ev(content(strings.Repeat("x", 5<<20))))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, []string{"delta"}, shape(events))
	})
}

func TestDecode_TenThousandSmallEvents(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(ev(runStarted) + ev(msgStart))
	for range 10_000 {
		sb.WriteString(ev(content("ab")))
	}
	sb.WriteString(ev(msgEnd) + ev(runFinished))

	events, err := decode(sb.String())

	require.NoError(t, err)
	require.Len(t, events, 10_001)
	assert.Equal(t, strings.Repeat("ab", 10_000), events[10_000].Text)
}

func TestDecode_ARunErrorIsAlwaysTheAgentsError(t *testing.T) {
	tests := []struct {
		name       string
		event      string
		wantDetail string
	}{
		{"an empty message", `{"type":"RUN_ERROR","message":""}`, ""},
		{"no message", `{"type":"RUN_ERROR"}`, ""},
		{"a null message", `{"type":"RUN_ERROR","message":null}`, ""},
		{"a message that is not a string", `{"type":"RUN_ERROR","message":{"code":500}}`, ""},
		{"a number as message", `{"type":"RUN_ERROR","message":42}`, ""},
		{"a message with an error code", `{"type":"RUN_ERROR","message":"boom","code":"E1"}`, "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decode(ev(runStarted) + ev(tc.event) + ev(runFinished))

			agentErr, ok := errors.AsType[*domain.AgentError](err)
			require.True(t, ok, "the agent said it failed: %v", err)
			assert.Equal(t, tc.wantDetail, agentErr.Detail)
		})
	}
}

func TestDecode_ARunErrorEndsTheStreamEvenWithMoreAfterIt(t *testing.T) {
	events, err := decode(ev(runStarted) + ev(`{"type":"RUN_ERROR","message":"x"}`) + ev(msgStart) + ev(content("nunca")) + ev(runFinished))

	require.Error(t, err)
	assert.Empty(t, events)
}

func TestDecode_ToolCalls(t *testing.T) {
	events, err := decode(ev(runStarted) +
		ev(`{"type":"TOOL_CALL_START","toolCallName":"a"}`) +
		ev(`{"type":"TOOL_CALL_START"}`) +
		ev(`{"type":"TOOL_CALL_START","toolCallName":"ñandú 🦤"}`) +
		ev(runFinished))

	require.NoError(t, err)
	assert.Equal(t, []string{"tool:a", "tool:", "tool:ñandú 🦤"}, shape(events))
}

// cardStream is a run that carries the CUSTOM events given.
func cardStream(customs ...string) string {
	sse := ev(runStarted)
	for _, c := range customs {
		sse += ev(c)
	}
	return sse + ev(runFinished)
}

func approval(value string) string {
	return `{"type":"CUSTOM","name":"ACTION_REQUEST","value":` + value + `}`
}

func contact(value string) string {
	return `{"type":"CUSTOM","name":"CONTACT_REQUEST","value":` + value + `}`
}

func TestDecode_Runs(t *testing.T) {
	t.Run("a card carries the run that was started before it", func(t *testing.T) {
		sse := ev(`{"type":"RUN_STARTED","runId":"first"}`) +
			ev(approval(`{"actionType":"approval","approvalId":"a1"}`)) +
			ev(`{"type":"RUN_STARTED","runId":"second"}`) +
			ev(approval(`{"actionType":"approval","approvalId":"a2"}`)) + ev(runFinished)

		events, err := decode(sse)

		require.NoError(t, err)
		cards := cardsOf(events)
		require.Len(t, cards, 2)
		assert.Equal(t, "first", cards[0].RunID)
		assert.Equal(t, "second", cards[1].RunID, "the latest RUN_STARTED is the run that makes the next card")
	})
	t.Run("a card before any RUN_STARTED has no run", func(t *testing.T) {
		events, err := decode(ev(approval(`{"actionType":"approval","approvalId":"a1"}`)) + ev(runFinished))

		require.NoError(t, err)
		require.Len(t, cardsOf(events), 1)
		assert.Empty(t, cardsOf(events)[0].RunID)
	})
}

func TestDecode_TwoApprovalCardsInOneRunAreBothDeliveredInOrder(t *testing.T) {
	events, err := decode(cardStream(
		approval(`{"actionType":"approval","approvalId":"a1","preview":{"amount":10,"currency":"PEN"}}`),
		approval(`{"actionType":"approval","approvalId":"a2","preview":{"amount":20,"currency":"USD"}}`),
	))

	require.NoError(t, err)
	cards := cardsOf(events)
	require.Len(t, cards, 2)
	assert.Equal(t, []string{"a1", "a2"}, []string{cards[0].ApprovalID, cards[1].ApprovalID})
	assert.Equal(t, []float64{10, 20}, []float64{cards[0].Transfer.Amount, cards[1].Transfer.Amount})
	assert.Equal(t, "r1", cards[1].RunID)
}

func TestDecode_ApprovalCards(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    *domain.Transfer // nil when the event is not a card
		wantErr string
	}{
		{name: "an unknown actionType is not a card", value: `{"actionType":"survey","approvalId":"a1"}`},
		{name: "no actionType is not a card", value: `{"approvalId":"a1"}`},
		{name: "the actionType is case sensitive", value: `{"actionType":"Approval","approvalId":"a1"}`},
		{name: "a null value is not a card", value: `null`},
		{name: "an empty approvalId is refused", value: `{"actionType":"approval","approvalId":""}`, wantErr: "approvalId"},
		{name: "a missing approvalId is refused", value: `{"actionType":"approval"}`, wantErr: "approvalId"},
		{name: "a null approvalId is refused", value: `{"actionType":"approval","approvalId":null}`, wantErr: "approvalId"},
		{name: "an approvalId that is a number is malformed", value: `{"actionType":"approval","approvalId":7}`, wantErr: "malformed"},
		{name: "a missing preview is a card with an empty transfer", value: `{"actionType":"approval","approvalId":"a1"}`, want: &domain.Transfer{}},
		{name: "a null preview is a card with an empty transfer", value: `{"actionType":"approval","approvalId":"a1","preview":null}`, want: &domain.Transfer{}},
		{name: "a preview that is a string is malformed", value: `{"actionType":"approval","approvalId":"a1","preview":"x"}`, wantErr: "malformed"},
		{name: "an amount as a JSON string is malformed: the user would approve a transfer of an unknown sum", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":"20"}}`, wantErr: "malformed"},
		{name: "a null amount is zero", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":null}}`, want: &domain.Transfer{}},
		{name: "an amount that is too big for a float is malformed", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":1e999}}`, wantErr: "malformed"},
		{name: "a decimal amount is kept", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":12.5,"currency":"PEN"}}`, want: &domain.Transfer{Amount: 12.5, Currency: "PEN"}},
		{name: "a huge amount is kept as is", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":1e15}}`, want: &domain.Transfer{Amount: 1e15}},
		{name: "a negative amount is kept as is: the service has the last word on it", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":-5}}`, want: &domain.Transfer{Amount: -5}},
		{name: "a zero amount is kept", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":0}}`, want: &domain.Transfer{}},
		{name: "a missing currency is empty", value: `{"actionType":"approval","approvalId":"a1","preview":{"amount":3}}`, want: &domain.Transfer{Amount: 3}},
		{
			name:  "HTML and emoji in the destination are kept verbatim: the page escapes them",
			value: `{"actionType":"approval","approvalId":"a1","preview":{"destinationName":"<img src=x onerror=alert(1)> 🦤","destinationPhone":"+51 (987) 654-321","message":"a\nb \"q\""}}`,
			want:  &domain.Transfer{DestinationName: "<img src=x onerror=alert(1)> 🦤", DestinationPhone: "+51 (987) 654-321", Message: "a\nb \"q\""},
		},
		{name: "unknown fields are ignored", value: `{"actionType":"approval","approvalId":"a1","actions":[{"url":"/x"}],"future":{"a":1},"preview":{"fee":1}}`, want: &domain.Transfer{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decode(cardStream(approval(tc.value)))

			if tc.wantErr != "" {
				require.ErrorIs(t, err, domain.ErrAgentUnavailable)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			cards := cardsOf(events)
			if tc.want == nil {
				assert.Empty(t, events)
				return
			}
			require.Len(t, cards, 1)
			assert.Equal(t, domain.InteractionApproval, cards[0].Kind)
			assert.Equal(t, domain.InteractionPending, cards[0].State)
			assert.Equal(t, *tc.want, cards[0].Transfer)
		})
	}
}

func TestDecode_ContactCards(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantQuery string
		wantMsg   string
		wantErr   string
	}{
		{name: "with the query only", value: `{"requestId":"q","query":"Maria"}`, wantQuery: "Maria"},
		{name: "with the agent reference only", value: `{"requestId":"q","agentReference":"Ana"}`, wantQuery: "Ana"},
		{name: "with both, different: the reference is what the BFF wants back", value: `{"requestId":"q","query":"ana","agentReference":"Ana Ruiz"}`, wantQuery: "Ana Ruiz"},
		{name: "with an empty reference the query is used", value: `{"requestId":"q","query":"ana","agentReference":""}`, wantQuery: "ana"},
		{name: "with neither it is refused", value: `{"requestId":"q"}`, wantErr: "query"},
		{name: "with both empty it is refused", value: `{"query":"","agentReference":""}`, wantErr: "query"},
		{name: "without a message the prompt is empty", value: `{"requestId":"q","query":"Maria"}`, wantQuery: "Maria", wantMsg: ""},
		{name: "a message is the prompt, verbatim", value: `{"query":"M","message":"<b>¿Es ella?</b> 🙂"}`, wantQuery: "M", wantMsg: "<b>¿Es ella?</b> 🙂"},
		{name: "a value that is a string is malformed", value: `"Maria"`, wantErr: "malformed"},
		{name: "a value that is a list is malformed", value: `["Maria"]`, wantErr: "malformed"},
		{name: "a null value has no query", value: `null`, wantErr: "query"},
		{name: "a query that is a number is malformed", value: `{"query":5}`, wantErr: "malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decode(cardStream(contact(tc.value)))

			if tc.wantErr != "" {
				require.ErrorIs(t, err, domain.ErrAgentUnavailable)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			cards := cardsOf(events)
			require.Len(t, cards, 1)
			assert.Equal(t, domain.InteractionContact, cards[0].Kind)
			assert.Equal(t, tc.wantQuery, cards[0].Query)
			assert.Equal(t, tc.wantMsg, cards[0].Prompt)
			assert.Equal(t, "r1", cards[0].RunID)
		})
	}
}

func TestDecode_CustomEventsOfOtherShapes(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		wantErr string
	}{
		{name: "a null name is no card", event: `{"type":"CUSTOM","name":null,"value":{"a":1}}`},
		{name: "no name is no card", event: `{"type":"CUSTOM","value":{"a":1}}`},
		{name: "a value that is null is no card", event: `{"type":"CUSTOM","name":"SOMETHING","value":null}`},
		{name: "a value that is a string is no card", event: `{"type":"CUSTOM","name":"SOMETHING","value":"x"}`},
		{name: "no value at all is no card", event: `{"type":"CUSTOM","name":"SOMETHING"}`},
		{name: "the name is case sensitive", event: `{"type":"CUSTOM","name":"action_request","value":{"actionType":"approval","approvalId":"a"}}`},
		{name: "an ACTION_REQUEST without a value is malformed", event: `{"type":"CUSTOM","name":"ACTION_REQUEST"}`, wantErr: "ACTION_REQUEST"},
		{name: "a CONTACT_REQUEST without a value is malformed", event: `{"type":"CUSTOM","name":"CONTACT_REQUEST"}`, wantErr: "CONTACT_REQUEST"},
		{name: "an ACTION_REQUEST whose value is a string is malformed", event: approval(`"x"`), wantErr: "ACTION_REQUEST"},
		{name: "a name that is a number is malformed", event: `{"type":"CUSTOM","name":5}`, wantErr: "malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decode(cardStream(tc.event))

			if tc.wantErr != "" {
				require.ErrorIs(t, err, domain.ErrAgentUnavailable)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, events)
		})
	}
}

// TestDecode_TheConsumerCanStopAtAnyPoint stops reading after every event of a run that has all kinds of them.
func TestDecode_TheConsumerCanStopAtAnyPoint(t *testing.T) {
	sse := ev(runStarted) +
		ev(`{"type":"TOOL_CALL_START","toolCallName":"t"}`) +
		ev(approval(`{"actionType":"approval","approvalId":"a1"}`)) +
		ev(contact(`{"query":"Maria"}`)) +
		ev(msgStart) + ev(content("uno")) + ev(content("dos")) + ev(msgEnd) +
		ev(msgStart) + ev(content("tres")) + ev(runFinished)
	all, err := decode(sse)
	require.NoError(t, err)
	require.Equal(t, []string{"tool:t", "interaction", "interaction", "delta", "delta", "final:unodos", "delta", "final:tres"}, shape(all))

	for stopAfter := 1; stopAfter <= len(all); stopAfter++ {
		t.Run(shape(all)[stopAfter-1], func(t *testing.T) {
			var seen []domain.AgentEvent
			for e, err := range aguibff.Decode(strings.NewReader(sse)) {
				require.NoError(t, err)
				seen = append(seen, e)
				if len(seen) == stopAfter {
					break
				}
			}

			assert.Equal(t, all[:stopAfter], seen)
		})
	}
}

func TestDecode_TheConsumerCanStopOnTheErrorsToo(t *testing.T) {
	for name, sse := range map[string]string{
		"a run error":         ev(runStarted) + ev(`{"type":"RUN_ERROR","message":"x"}`),
		"a broken event":      ev(runStarted) + ev("{"),
		"a stream that stops": ev(runStarted),
		"a long line":         "data: " + strings.Repeat("x", 5<<20) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, err := range aguibff.Decode(strings.NewReader(sse)) {
				require.Error(t, err)
				break
			}
		})
	}
}

type failingReader struct{ after string }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.after == "" {
		return 0, errors.New("connection reset")
	}
	n := copy(p, f.after)
	f.after = f.after[n:]
	return n, nil
}

func TestDecode_AReaderThatFailsIsAnUnavailableAgentOrAnIncompleteAnswer(t *testing.T) {
	t.Run("before any text", func(t *testing.T) {
		_, err := collect(aguibff.Decode(&failingReader{after: ev(runStarted)}))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Contains(t, err.Error(), "connection reset")
	})
	t.Run("after some text", func(t *testing.T) {
		events, err := collect(aguibff.Decode(&failingReader{after: ev(runStarted) + ev(msgStart) + ev(content("a"))}))

		require.ErrorIs(t, err, domain.ErrIncompleteStream, "a connection that dies mid-answer is an answer that stopped short")
		assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Contains(t, err.Error(), "connection reset")
		assert.Equal(t, []string{"delta"}, shape(events))
	})
	t.Run("an empty reader", func(t *testing.T) {
		_, err := collect(aguibff.Decode(io.NopCloser(strings.NewReader(""))))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	})
}

func TestDecode_TheTextIsValidUTF8WhateverTheChunks(t *testing.T) {
	events, err := decode(reply("¿Qué tal? 👋 日本語"))

	require.NoError(t, err)
	assert.True(t, utf8.ValidString(events[1].Text))
}

func TestDecode_ABOMIsDroppedSoTheFirstEventIsNotLost(t *testing.T) {
	// The first event is RUN_STARTED: without it the card would not know its run.
	sse := "\xEF\xBB\xBF" + ev(`{"type":"RUN_STARTED","runId":"first-run"}`) + ev(approval(`{"actionType":"approval","approvalId":"a1"}`)) + ev(runFinished)

	events, err := decode(sse)

	require.NoError(t, err)
	require.Len(t, cardsOf(events), 1)
	assert.Equal(t, "first-run", cardsOf(events)[0].RunID)
}

func TestDecode_AnEventWithoutDataIsAKeepAlive(t *testing.T) {
	events, err := decode("data:\n\ndata: \n\n" + reply("hola"))

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:hola"}, shape(events))
}

func TestDecode_ABrokenEventAtTheEndOfTheStreamIsAnError(t *testing.T) {
	_, err := decode(ev(runStarted) + "data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"delta\":\"cut")

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "malformed")
}

func TestDecode_AReaderThatFailsInTheMiddleOfAnEventDoesNotDeliverHalfOfIt(t *testing.T) {
	events, err := collect(aguibff.Decode(&failingReader{after: ev(runStarted) + "data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"delta\":\"a\"}\n"}))

	require.Error(t, err)
	assert.Empty(t, events, "an event ends with its blank line: this one never did")
}

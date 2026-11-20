package aguibff_test

import (
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
)

// collect drains a decoder: the events it yielded and the error it ended with.
func collect(seq iter.Seq2[domain.AgentEvent, error]) ([]domain.AgentEvent, error) {
	var events []domain.AgentEvent
	for ev, err := range seq {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(body)
}

// kinds is the shape of a reply: its event kinds, with the tool names and the texts of the final segments.
func shape(events []domain.AgentEvent) []string {
	var out []string
	for _, ev := range events {
		switch ev.Kind {
		case domain.AgentToolCall:
			out = append(out, "tool:"+ev.Tool)
		case domain.AgentFinal:
			out = append(out, "final:"+ev.Text)
		case domain.AgentDelta:
			out = append(out, "delta")
		default:
			out = append(out, string(ev.Kind))
		}
	}
	return out
}

func TestDecode_RealStreams(t *testing.T) {
	tests := []struct {
		fixture string
		want    []string
	}{
		{"text.sse", []string{"delta", "final:¡Hola! ¿En qué te ayudo?\nTe dejo algunas opciones para comenzar:\nYapeos\nPago de servicios\nCobro individual\nCobros pendientes\nAsesoría financiera"}},
		{"guard.sse", []string{"delta", "final:No puedo ayudarte con esa solicitud. ¿Intentamos con otra?"}},
		{"contact-ask.sse", []string{"tool:transfer_to_agent", "tool:transfer_to_agent", "delta", "final:¿A quién necesitas yapearle y cuánto?"}},
		{"limit.sse", []string{"tool:transfer_to_agent", "tool:validate_p2p_amount", "delta", "final:El máximo para cualquier usuario es S/500; indícame un monto de hasta S/500."}},
		{"approval.sse", []string{
			"tool:transfer_to_agent", "tool:validate_p2p_amount", "tool:request_contact_from_client", "tool:draft_transfer", "tool:request_transfer_approval",
			"interaction", "delta", "final:Revisa tu yapeo y confirma si todo está bien. Si necesitas, puedes cambiar los datos.",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			events, err := collect(aguibff.Decode(strings.NewReader(fixture(t, tc.fixture))))

			require.NoError(t, err)
			assert.Equal(t, tc.want, shape(events))
		})
	}
}

func TestDecode_TheFinalSegmentIsWhatTheDeltasAdd(t *testing.T) {
	sse := "data: {\"type\":\"RUN_STARTED\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m1\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m1\",\"delta\":\"Hola, \"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m1\",\"delta\":\"mundo\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_END\",\"messageId\":\"m1\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m2\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m2\",\"delta\":\"otra\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_END\",\"messageId\":\"m2\"}\n\n" +
		"data: {\"type\":\"RUN_FINISHED\"}\n\n"

	events, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.NoError(t, err)
	assert.Equal(t, []domain.AgentEvent{
		{Kind: domain.AgentDelta, Text: "Hola, "},
		{Kind: domain.AgentDelta, Text: "mundo"},
		{Kind: domain.AgentFinal, Text: "Hola, mundo"},
		{Kind: domain.AgentDelta, Text: "otra"},
		{Kind: domain.AgentFinal, Text: "otra"},
	}, events, "each message is its own segment, and its final replaces its deltas")
}

func TestDecode_RunFinishedBeforeTheLastMessageEndKeepsTheText(t *testing.T) {
	sse := "data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"texto\"}\n\n" +
		"data: {\"type\":\"RUN_FINISHED\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_END\",\"messageId\":\"m\"}\n\n"

	events, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:texto"}, shape(events), "the BFF closes the run before the last message: nothing is lost")
}

func TestDecode_ARunErrorIsTheAgentsError(t *testing.T) {
	sse := "data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"a medias\"}\n\n" +
		"data: {\"type\":\"RUN_ERROR\",\"message\":\"Agent run failed\"}\n\n"

	events, err := collect(aguibff.Decode(strings.NewReader(sse)))

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	require.True(t, ok, "the error is the agent's own: %v", err)
	assert.Equal(t, "Agent run failed", agentErr.Detail)
	assert.Equal(t, []string{"delta"}, shape(events), "what arrived before the error is delivered")
}

func TestDecode_AStreamCutAfterSomeTextIsAnAnswerThatStoppedShort(t *testing.T) {
	sse := "data: {\"type\":\"RUN_STARTED\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m\"}\n\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"corta\"}\n\n"

	events, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.ErrorIs(t, err, domain.ErrIncompleteStream)
	assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "RUN_FINISHED")
	assert.Equal(t, []string{"delta"}, shape(events), "what arrived is delivered")
}

func TestDecode_AStreamThatEndsBeforeAnyTextIsAnUnavailableAgent(t *testing.T) {
	sse := "data: {\"type\":\"RUN_STARTED\"}\n\n"

	_, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "RUN_FINISHED")
}

func TestDecode_ABrokenEventIsAnError(t *testing.T) {
	_, err := collect(aguibff.Decode(strings.NewReader("data: {\"type\":\n\n")))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "malformed")
}

func TestDecode_WhatIsNotAnEventIsLeftAlone(t *testing.T) {
	sse := ": a comment\r\n" +
		"event: agent.message\r\n" +
		"data: {\"type\":\"SOMETHING_NEW\",\"x\":1}\r\n\r\n" +
		"data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m\"}\r\n\r\n" +
		"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"hola\"}\r\n\r\n" +
		"data: {\"type\":\"TEXT_MESSAGE_END\",\"messageId\":\"m\"}\r\n\r\n" +
		"data: {\"type\":\"RUN_FINISHED\"}\r\n\r\n"

	events, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:hola"}, shape(events), "comments, other fields, unknown events and CRLF do not matter")
}

func TestDecode_TheConsumerCanStopEarly(t *testing.T) {
	var seen int
	for range aguibff.Decode(strings.NewReader(fixture(t, "approval.sse"))) {
		seen++
		break
	}
	assert.Equal(t, 1, seen)
}

func TestDecode_AHugeLineIsRefused(t *testing.T) {
	line := "data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"" + strings.Repeat("x", 5<<20) + "\"}\n\n"

	_, err := collect(aguibff.Decode(strings.NewReader(line)))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "too long")
}

func TestDecode_TheApprovalCardIsAnInteraction(t *testing.T) {
	events, err := collect(aguibff.Decode(strings.NewReader(fixture(t, "approval.sse"))))

	require.NoError(t, err)
	var cards []domain.Interaction
	for _, ev := range events {
		if ev.Kind == domain.AgentInteraction {
			cards = append(cards, ev.Interaction)
		}
	}
	require.Len(t, cards, 1)
	assert.Equal(t, domain.Interaction{
		Kind:       domain.InteractionApproval,
		State:      domain.InteractionPending,
		ApprovalID: "appr_28006196-bd87-4d46-931e-0f74a46b2c57",
		RunID:      "28df4c1f-77a3-499a-a310-e5bd9b359dbf",
		Transfer:   domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz", DestinationPhone: "987654321"},
	}, cards[0], "the card knows the run that made it: the service asks for it with every answer")
}

func TestDecode_ACustomEventThatIsNotACardIsLeftAlone(t *testing.T) {
	sse := "data: {\"type\":\"RUN_STARTED\",\"runId\":\"r\"}\n\n" +
		"data: {\"type\":\"CUSTOM\",\"name\":\"CONVERSATION_HANDOFF\",\"value\":{\"to\":\"x\"}}\n\n" +
		"data: {\"type\":\"CUSTOM\",\"name\":\"ACTION_REQUEST\",\"value\":{\"actionType\":\"something-else\",\"approvalId\":\"a\"}}\n\n" +
		"data: {\"type\":\"RUN_FINISHED\"}\n\n"

	events, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestDecode_ACardWithoutAnApprovalIsRefused(t *testing.T) {
	sse := "data: {\"type\":\"RUN_STARTED\",\"runId\":\"r\"}\n\n" +
		"data: {\"type\":\"CUSTOM\",\"name\":\"ACTION_REQUEST\",\"value\":{\"actionType\":\"approval\",\"preview\":{}}}\n\n"

	_, err := collect(aguibff.Decode(strings.NewReader(sse)))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "approvalId")
}

func TestDecode_TheContactRequestIsAnInteraction(t *testing.T) {
	events, err := collect(aguibff.Decode(strings.NewReader(fixture(t, "contact-request.sse"))))

	require.NoError(t, err)
	var cards []domain.Interaction
	for _, ev := range events {
		if ev.Kind == domain.AgentInteraction {
			cards = append(cards, ev.Interaction)
		}
	}
	require.Len(t, cards, 1)
	assert.Equal(t, domain.Interaction{
		Kind:      domain.InteractionContact,
		State:     domain.InteractionPending,
		RequestID: "4a2569dc-090d-42ec-a2aa-bc107ad724d2",
		RunID:     "fd92d122-4d82-402f-9572-772a8efb6b12",
		Query:     "Maria",
		Prompt:    "Confirma que sea la persona correcta antes de yapearle.",
	}, cards[0])
}

func TestDecode_AContactRequestUsesTheAgentReferenceAndNeedsOne(t *testing.T) {
	stream := func(value string) string {
		return "data: {\"type\":\"RUN_STARTED\",\"runId\":\"r\"}\n\n" +
			"data: {\"type\":\"CUSTOM\",\"name\":\"CONTACT_REQUEST\",\"value\":" + value + "}\n\n" +
			"data: {\"type\":\"RUN_FINISHED\"}\n\n"
	}

	events, err := collect(aguibff.Decode(strings.NewReader(stream(`{"requestId":"q","agentReference":"Ana","query":"ana","message":"m"}`))))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "Ana", events[0].Interaction.Query, "the reference is what the BFF wants back")

	events, err = collect(aguibff.Decode(strings.NewReader(stream(`{"requestId":"q","query":"ana","message":"m"}`))))
	require.NoError(t, err)
	assert.Equal(t, "ana", events[0].Interaction.Query, "without a reference the query is used")

	_, err = collect(aguibff.Decode(strings.NewReader(stream(`{"requestId":"q","message":"m"}`))))
	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Contains(t, err.Error(), "query")
}

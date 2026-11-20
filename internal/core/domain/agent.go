package domain

import (
	"errors"
	"uuid"
)

// AgentRequest is one user turn sent to the agent. The agent keeps its own
// session per conversation, keyed by the conversation ID.
type AgentRequest struct {
	UserID         uuid.UUID
	ConversationID uuid.UUID
	Text           string
}

// AgentEventKind says what an AgentEvent carries.
type AgentEventKind string

// Agent event kinds.
const (
	// AgentDelta is a piece of the reply, to append to the segment in progress.
	AgentDelta AgentEventKind = "delta"
	// AgentFinal is the complete text of one segment of the reply. It replaces
	// the deltas of that segment; clients must never append both.
	AgentFinal AgentEventKind = "final"
	// AgentUsage reports token counts for one model call.
	AgentUsage AgentEventKind = "usage"
	// AgentToolCall says the agent called a tool.
	AgentToolCall AgentEventKind = "tool_call"
	// AgentInteraction says the agent put a card in the reply for the user to answer.
	AgentInteraction AgentEventKind = "interaction"
)

// AgentEvent is one thing the agent did while producing a reply. Reasoning
// ("thought") parts never become events.
type AgentEvent struct {
	Kind  AgentEventKind
	Text  string // AgentDelta, AgentFinal
	Usage Usage  // AgentUsage
	Tool  string // AgentToolCall
	// Interaction is the card of an AgentInteraction.
	Interaction Interaction
}

// ErrNotSent says that a request to the agent's service certainly did not reach it (the token could not be had, the connection was
// refused, the token was refused before anything was done): nothing was done there, so a card can be given back to be answered
// again. An error without it may have reached the service, and what it was asked may have been done.
var ErrNotSent = errors.New("the request did not reach the agent's service")

// ErrIncompleteStream means the agent's stream ended without a complete reply.
var ErrIncompleteStream = errors.New("agent stream ended before the reply was complete")

// AgentError is an error the agent itself reported. Detail is for logs; it can
// contain internals and must not be shown to users.
type AgentError struct {
	Detail string
}

// Error implements error.
func (e *AgentError) Error() string { return "agent reported an error: " + e.Detail }

package domain

import (
	"errors"
	"strings"
	"uuid"
)

// conversationIDPrefix opens the id of every conversation: the agent accepts no other.
const conversationIDPrefix = "conv_v1_"

// ErrInvalidConversationID means a string is not the id of a conversation: "conv_v1_" and a canonical, lowercase UUID v4.
var ErrInvalidConversationID = errors.New("not a conversation id")

// NewConversationID is the id of a new chat. The UI makes it once, when the chat starts, and sends it as the threadId of every request
// of the chat; the BFF hands it to the agent as the contextId, so it is the id the conversation has in the history.
func NewConversationID() string { return conversationIDPrefix + uuid.NewV4().String() }

// ParseConversationID returns s when it is the id of a conversation: "conv_v1_" and a canonical, lowercase UUID v4 (44 characters in
// all), which is the only shape the agent accepts. Anything else, however close, is ErrInvalidConversationID.
func ParseConversationID(s string) (string, error) {
	body, ok := strings.CutPrefix(s, conversationIDPrefix)
	if !ok {
		return "", ErrInvalidConversationID
	}
	parsed, err := uuid.Parse(body)
	// uuid.Parse also takes other spellings (braces, a urn, no hyphens, upper case): the canonical one is the only one allowed.
	if err != nil || parsed.String() != body || body[14] != '4' || !strings.ContainsRune("89ab", rune(body[19])) {
		return "", ErrInvalidConversationID
	}
	return s, nil
}

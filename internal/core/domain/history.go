package domain

import (
	"errors"
	"time"
)

// What the history can fail with. ErrNotFound is the answer for a conversation that does not exist or is someone else's.
var (
	// ErrHistoryUnavailable means the history could not be read: it did not answer, answered with a failure, or answered with
	// something that is not what it promises.
	ErrHistoryUnavailable = errors.New("history unavailable")
	// ErrInvalidPage means a page request is not one the history takes: a cursor that is not one of its cursors, both directions
	// at once, or a size outside 1 to MaxPageSize.
	ErrInvalidPage = errors.New("invalid page request")
)

// Limits of a page of the history.
const (
	// MaxPageSize is the most conversations or messages one request returns.
	MaxPageSize = 100
	// maxCursorBytes is the longest cursor taken: the history's own are about 80 bytes.
	maxCursorBytes = 256
)

// PageRequest says which page of the history is wanted. The zero value is the newest page of the default size. A cursor is what a
// previous page gave (Older or Newer): it is opaque, and it is passed back as it came.
type PageRequest struct {
	// Before asks for the page that is older than the cursor, After for the page that is newer. Only one of them is set.
	Before string
	After  string
	// Size is how many items the page holds. Zero is the history's default (the most it returns).
	Size int
}

// Validate reports ErrInvalidPage for a request the history would refuse, so that it is not sent.
func (p PageRequest) Validate() error {
	switch {
	case p.Before != "" && p.After != "":
		return ErrInvalidPage
	case p.Size < 0 || p.Size > MaxPageSize:
		return ErrInvalidPage
	case !validCursor(p.Before) || !validCursor(p.After):
		return ErrInvalidPage
	}
	return nil
}

// validCursor accepts what is empty or printable ASCII without spaces, up to maxCursorBytes.
func validCursor(c string) bool {
	if len(c) > maxCursorBytes {
		return false
	}
	for i := range len(c) {
		if c[i] <= ' ' || c[i] >= 0x7f {
			return false
		}
	}
	return true
}

// ConversationSummary is one row of the history's list: what a sidebar shows of a conversation.
type ConversationSummary struct {
	// ID is the conversation's id, the one the UI made when the chat started.
	ID string
	// Cursor is the opaque position of the row in the list.
	Cursor       string
	MessageCount int
	// FirstAt and LastAt are the times of the first and the last message.
	FirstAt time.Time
	LastAt  time.Time
	// FirstMessage is the first thing the user wrote, cut by the history at 200 characters. It is empty when the user wrote
	// nothing. It is text, never markup.
	FirstMessage          string
	FirstMessageTruncated bool
}

// ConversationList is a page of the list, newest conversation first. Older and Newer are the cursors of the neighbouring pages,
// empty where there is none.
type ConversationList struct {
	Conversations []ConversationSummary
	Older         string
	Newer         string
}

// TranscriptMessage is one message of a conversation as the history has it.
type TranscriptMessage struct {
	// ID is the message's id and its position among the others: it is unique in the conversation.
	ID string
	// Seq is the sequence of the event that produced it. Gaps are normal.
	Seq  int
	Role Role
	Text string
	// Truncated is true when the history holds only the start of the text.
	Truncated bool
	At        time.Time
	// Flow is the flow the message took place in, such as p2p. Empty outside any flow.
	Flow string
}

// Transcript is a page of a conversation, oldest message first. Older and Newer are the cursors of the neighbouring pages, empty
// where there is none.
type Transcript struct {
	ConversationID string
	Messages       []TranscriptMessage
	Older          string
	Newer          string
}

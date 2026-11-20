package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"iape-ui/internal/core/domain"
)

const (
	// historyPageSize is how many conversations a page of the Historial holds.
	historyPageSize = 30
	// titleChars is how much of the first message a row shows. The row cuts the rest by itself (CSS) too; this keeps what is sent short.
	titleChars = 90
	noTitle    = "(no message)"
	historyOff = "The history is not available right now."
)

// historyItem is a row of the Historial.
type historyItem struct {
	ID     string
	Title  string // the first message of the conversation, in one line
	When   string // when it last had a message, in the place of the history
	Count  string // how many messages, as a person reads it
	Active bool   // the conversation on screen
}

// historyView is what the Historial templates draw.
type historyView struct {
	Items    []historyItem
	MoreURL  string // where the older conversations are, empty when there are none
	Error    string
	RetryURL string
}

// historyURL is where the Historial is loaded from: its newest page, or the one that the cursor leads to. active is the conversation
// on screen, which its row marks.
func historyURL(active, before string) string {
	q := url.Values{}
	if active != "" {
		q.Set("active", active)
	}
	if before != "" {
		q.Set("before", before)
	}
	if len(q) == 0 {
		return "/history"
	}
	return "/history?" + q.Encode()
}

// historyList answers with the Historial of the signed-in user: the conversations of their account, the newest first, or the page
// that the cursor leads to, which is only what to add. The page never sees the account and never talks to the history itself. A
// history that cannot be read is said in the list's place, with a way to try again: the chat does not depend on it.
func (a *App) historyList(w http.ResponseWriter, r *http.Request) {
	if !a.historyOn() {
		http.NotFound(w, r)
		return
	}
	user, _ := UserFrom(r.Context())
	before, active := r.URL.Query().Get("before"), r.URL.Query().Get("active")

	list, err := a.svc.History.Conversations(r.Context(), user.ID, domain.PageRequest{Before: before, Size: historyPageSize})
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, domain.ErrInvalidPage):
		http.Error(w, "That page of the history does not exist.", http.StatusBadRequest)
		return
	case errors.Is(err, domain.ErrHistoryUnavailable):
		// 200: htmx swaps only what answers well, and the list has to say why it is not there.
		a.block(w, r, http.StatusOK, "partial:history", historyView{Error: historyOff, RetryURL: historyURL(active, before)})
		return
	case err != nil:
		a.serverError(w, r, err)
		return
	}

	view := historyView{Items: make([]historyItem, 0, len(list.Conversations))}
	for _, c := range list.Conversations {
		view.Items = append(view.Items, historyItem{
			ID: c.ID, Title: rowTitle(c), When: a.shortTime(c.LastAt), Count: messageCount(c.MessageCount), Active: c.ID == active,
		})
	}
	if list.Older != "" {
		view.MoreURL = historyURL(active, list.Older)
	}
	if before != "" {
		a.block(w, r, http.StatusOK, "partial:history_page", view)
		return
	}
	a.block(w, r, http.StatusOK, "partial:history", view)
}

// rowTitle is what a row says of its conversation: the first thing the user wrote, in one line and not too long.
func rowTitle(c domain.ConversationSummary) string {
	line := strings.Join(strings.Fields(plain(c.FirstMessage)), " ")
	if line == "" {
		return noTitle
	}
	if utf8.RuneCountInString(line) > titleChars {
		return string([]rune(line)[:titleChars]) + "…"
	}
	if c.FirstMessageTruncated {
		return line + "…"
	}
	return line
}

func messageCount(n int) string {
	if n == 1 {
		return "1 message"
	}
	return strconv.Itoa(n) + " messages"
}

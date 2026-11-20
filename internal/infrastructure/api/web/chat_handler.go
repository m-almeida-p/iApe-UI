package web

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"iape-ui/internal/core/domain"
)

// transcriptPageSize is how many messages of a conversation a page of the transcript holds: what the history gives at most.
const transcriptPageSize = domain.MaxPageSize

// messageView is what the message templates draw.
type messageView struct {
	ID             string
	ConversationID string
	Role           string
	Status         string
	Text           string        // a user's message: plain text
	Body           template.HTML // a reply: rendered, sanitized Markdown
	Error          string
	// Streaming is true while a reply is unfinished: its wrapper opens the
	// event stream, which fills it in.
	Streaming bool
	// Raw is the text of a finished reply as it is, Markdown, for the Copy button; empty when there is nothing to copy.
	Raw              string
	TTFTMs           int64
	DurationMs       int64
	PromptTokens     int
	CompletionTokens int
	ToolCalls        string
	// Card is the card the agent put in the reply, if it did.
	Card *cardView
}

// messageView draws a live message: what the user just wrote, or the reply being written.
func (a *App) messageView(m domain.Message) messageView {
	v := messageView{
		ID:             m.ID.String(),
		ConversationID: m.ConversationID,
		Role:           string(m.Role),
		Status:         string(m.Status),
		Error:          m.Error,
	}
	if m.Role == domain.RoleUser {
		v.Text = m.Content
		return v
	}
	if m.Status.Terminal() {
		v.Body = a.md.RenderFinal(m.Content)
		v.Raw = m.Content
		v.TTFTMs, v.DurationMs = m.Meta.TTFTMs, m.Meta.DurationMs
		v.PromptTokens, v.CompletionTokens = m.Meta.Usage.Prompt, m.Meta.Usage.Completion
		v.ToolCalls = strings.Join(m.Meta.ToolCalls, ",")
	} else {
		v.Body = a.md.Render(m.Content)
	}
	v.Streaming = !m.Status.Terminal()
	// A card is for a reply that completed: one that failed or was stopped is not one to pay or choose on, even if it had one.
	if m.Status == domain.StatusComplete {
		v.Card = newCardView(m)
	}
	return v
}

// transcriptView draws a message of the history. It is finished, and it has no card: a card is live in this server only.
func (a *App) transcriptView(convID string, m domain.TranscriptMessage) messageView {
	v := messageView{ID: "h" + m.ID, ConversationID: convID, Role: string(m.Role), Status: string(domain.StatusComplete)}
	text := plain(m.Text) // what the history holds is data: no control or direction-changing characters reach the page
	if m.Role == domain.RoleUser {
		v.Text = text
		return v
	}
	v.Body = a.md.RenderFinal(text)
	v.Raw = text
	return v
}

// chatPage is the page of a chat: the one that is being started, or a conversation of the history.
type chatPage struct {
	basePage
	// ConversationID is the chat on screen: the one being started, or the one opened.
	ConversationID string
	// HistoryOn is whether the sidebar has the Historial: there is a history to read. HistoryURL is where its list is loaded from.
	HistoryOn  bool
	HistoryURL string
	// CanWrite is whether the composer is there. A conversation whose id is not of the form the agent takes can be read, not continued.
	CanWrite bool
	Messages []messageView
	// EarlierURL is where the messages before these are, empty when these are the first.
	EarlierURL string
	// Note says what could not be done with the page (the history was not read).
	Note  string
	Draft string // what the visitor typed, when the page is shown again after a problem
	Error string
}

func (a *App) historyOn() bool { return a.svc.History != nil }

// renderChat renders the chat page.
func (a *App) renderChat(w http.ResponseWriter, r *http.Request, status int, page chatPage) {
	page.basePage = a.base(r)
	page.HistoryOn = a.historyOn()
	page.HistoryURL = historyURL(page.ConversationID, "")
	if err := a.renderer.Page(w, r, status, "chat", page); err != nil {
		a.serverError(w, r, err)
	}
}

// newChatPage is the empty state: a chat that has not started, with the id it will have, which only this user can write to.
func (a *App) newChatPage(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())
	a.renderChat(w, r, http.StatusOK, chatPage{ConversationID: a.svc.Chat.NewConversation(user.ID), CanWrite: true})
}

// conversationPage shows a conversation: what the history has of it, and a composer to go on. One that was started here and the
// history does not know yet is shown empty. One that is not the user's, or that does not exist, is a 404.
func (a *App) conversationPage(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())
	id := r.PathValue("id")
	_, idErr := domain.ParseConversationID(id)
	page := chatPage{ConversationID: id, CanWrite: idErr == nil}

	if a.historyOn() {
		t, err := a.svc.History.Transcript(r.Context(), user.ID, id, domain.PageRequest{Size: transcriptPageSize})
		switch {
		case err == nil:
			page.Messages, page.EarlierURL = a.transcriptViews(id, t)
		case errors.Is(err, domain.ErrNotFound):
			if !a.svc.Chat.Owns(user.ID, id) {
				http.NotFound(w, r)
				return
			}
		case errors.Is(err, domain.ErrHistoryUnavailable):
			if !a.svc.Chat.Owns(user.ID, id) {
				page.Note, page.CanWrite = historyDown, false
				a.renderChat(w, r, http.StatusServiceUnavailable, page)
				return
			}
			page.Note = historyDown // a chat started here goes on, with what the history would show missing
		default:
			a.serverError(w, r, err)
			return
		}
	} else if !a.svc.Chat.Owns(user.ID, id) {
		http.NotFound(w, r)
		return
	}
	a.renderChat(w, r, http.StatusOK, page)
}

// historyDown is what the page says when the history cannot be read.
const historyDown = "The history is not available right now. Try again in a moment."

// transcriptViews draws a page of a transcript, and says where the page before it is.
func (a *App) transcriptViews(convID string, t domain.Transcript) ([]messageView, string) {
	views := make([]messageView, 0, len(t.Messages))
	for _, m := range t.Messages {
		views = append(views, a.transcriptView(convID, m))
	}
	earlier := ""
	if t.Older != "" {
		earlier = "/c/" + url.PathEscape(convID) + "/earlier?" + url.Values{"before": {t.Older}}.Encode()
	}
	return views, earlier
}

// earlierMessages answers with the page of the conversation before the one on screen, to put above it, and the button for the
// page before that. A cursor that is not one of the history's is a 400.
func (a *App) earlierMessages(w http.ResponseWriter, r *http.Request) {
	if !a.historyOn() {
		http.NotFound(w, r)
		return
	}
	user, _ := UserFrom(r.Context())
	id := r.PathValue("id")
	t, err := a.svc.History.Transcript(r.Context(), user.ID, id, domain.PageRequest{Before: r.URL.Query().Get("before"), Size: transcriptPageSize})
	switch {
	case errors.Is(err, domain.ErrInvalidPage):
		http.Error(w, "That page of the conversation does not exist.", http.StatusBadRequest)
		return
	case errors.Is(err, domain.ErrHistoryUnavailable):
		w.Header().Set("HX-Reswap", "none")
		http.Error(w, historyDown, http.StatusServiceUnavailable)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	views, earlier := a.transcriptViews(id, t)
	w.Header().Set("Cache-Control", "no-store")
	a.block(w, r, http.StatusOK, "partial:earlier", struct {
		Messages   []messageView
		EarlierURL string
	}{views, earlier})
}

// sendMessage adds a message to a conversation. htmx gets the two new bubbles to append (the reply's wrapper opens its stream) and
// the address of the conversation, which a chat that was just started now has.
func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, _ := UserFrom(r.Context())
	if !fromHTMX(r) { // a reply is read from a stream, which the page opens: without it there is nothing to send to
		if !a.svc.Chat.Owns(user.ID, id) {
			http.NotFound(w, r) // a conversation that is not theirs is answered as one that does not exist
			return
		}
		redirect(w, r, "/c/"+url.PathEscape(id))
		return
	}
	if !a.parseForm(w, r) {
		return
	}
	content := r.PostFormValue("content")

	turn, err := a.svc.Chat.Send(r.Context(), user.ID, id, content)
	switch {
	case errors.Is(err, domain.ErrInvalidConversationID):
		http.NotFound(w, r)
		return
	case errors.Is(err, domain.ErrInvalidState):
		a.composerProblem(w, r, http.StatusConflict, "Wait for the current reply to finish.")
		return
	case errors.Is(err, domain.ErrHistoryUnavailable), errors.Is(err, domain.ErrBusy):
		a.composerProblem(w, r, http.StatusServiceUnavailable, "That can't be sent right now. Try again in a moment.")
		return
	case err != nil:
		if ve, ok := errors.AsType[*domain.ValidationError](err); ok {
			a.composerProblem(w, r, http.StatusUnprocessableEntity, ve.Message)
			return
		}
		a.fail(w, r, err)
		return
	}

	var out strings.Builder
	for _, m := range []domain.Message{turn.User, turn.Reply} {
		html, err := a.renderer.Block("partial:message", a.messageView(m))
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		out.WriteString(html)
	}
	clear, err := a.renderer.Block("partial:composer_error", "")
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	out.WriteString(clear)
	// A chat that was just started is at /; from now on it is at its own address, which a reload opens.
	w.Header().Set("HX-Replace-Url", "/c/"+url.PathEscape(id))
	writeHTML(w, r, http.StatusOK, out.String())
}

// composerProblem reports a problem with what was typed: htmx updates only the composer's message line and leaves the conversation
// alone.
func (a *App) composerProblem(w http.ResponseWriter, r *http.Request, status int, message string) {
	html, err := a.renderer.Block("partial:composer_error", message)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if !fromHTMX(r) {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("HX-Reswap", "none")
	writeHTML(w, r, status, html)
}

// parseForm reads the form, answering 413 or 400 itself when it cannot.
func (a *App) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		if !writeBodyError(w, err) {
			http.Error(w, "bad form", http.StatusBadRequest)
		}
		return false
	}
	return true
}

func writeHTML(w http.ResponseWriter, r *http.Request, status int, html string) {
	sendHTML(w, r, status, []byte(html))
}

// shortTime is the time of a message or a conversation as a person reads it, in the place of the history: the hour for today, the
// day and the hour for any other.
func (a *App) shortTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	at, now := t.In(a.opts.Location), a.opts.Now().In(a.opts.Location)
	if at.Year() == now.Year() && at.YearDay() == now.YearDay() {
		return at.Format("15:04")
	}
	if at.Year() == now.Year() {
		return at.Format("2 Jan 15:04")
	}
	return at.Format("2 Jan 2006 15:04")
}

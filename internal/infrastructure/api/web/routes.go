// Package web is the HTTP driving adapter: routes, middleware, handlers and
// server lifecycle.
package web

import "net/http"

// Routes returns the route table behind the authentication check. Wrap it with
// Handler for the rest of the middleware.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.Handle("GET /static/{path...}", a.static)
	mux.HandleFunc("GET /favicon.ico", a.static.favicon)

	signIn := limited(a.logins, byClient, a.tooManyLogins)
	send := limited(a.sends, byUser, a.tooManySends)
	answer := limited(a.answers, byUser, a.tooManyAnswers)

	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", signIn(a.login))
	mux.HandleFunc("POST /logout", a.logout)

	mux.HandleFunc("GET /{$}", a.newChatPage)
	mux.HandleFunc("GET /history", a.historyList)
	mux.HandleFunc("GET /c/{id}", a.conversationPage)
	mux.HandleFunc("GET /c/{id}/earlier", a.earlierMessages)
	mux.HandleFunc("POST /c/{id}/messages", send(a.sendMessage))
	mux.HandleFunc("GET /c/{id}/m/{mid}/stream", a.streamHandler)
	mux.HandleFunc("POST /c/{id}/m/{mid}/stop", a.stopMessage)
	mux.HandleFunc("GET /contacts", a.contactPicker)
	mux.HandleFunc("POST /contacts/confirm", answer(a.confirmCandidate))
	mux.HandleFunc("POST /c/{id}/m/{mid}/card/approve", answer(a.approveCard))
	mux.HandleFunc("POST /c/{id}/m/{mid}/card/reject", answer(a.rejectCard))
	mux.HandleFunc("POST /c/{id}/m/{mid}/card/otp", answer(a.confirmCode))
	mux.HandleFunc("POST /c/{id}/m/{mid}/card/contact", send(a.chooseContact))
	return a.authenticate(mux)
}

// healthz is the liveness probe: it never touches a dependency.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

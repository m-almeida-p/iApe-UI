package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/service"
	"iape-ui/internal/infrastructure/api/web/render"
)

// Services are the use cases the handlers call.
type Services struct {
	Auth service.Auth
	Chat service.Chat
	// History is the conversation history. It is nil when there is none (no HISTORY_BASE_URL): the page then has no Historial.
	History      service.History
	Interactions service.Interactions
}

// Options are the settings the handlers need.
type Options struct {
	// SecureCookies adds the Secure attribute to the session cookie (https).
	SecureCookies bool
	// Now is the clock; it is time.Now when nil. It is what "today" is when the hours of the history are shown.
	Now func() time.Time
	// Location is the place whose hours the history is shown in; it is UTC when nil.
	Location *time.Location
	// FlushInterval is the shortest time between streamed updates.
	FlushInterval time.Duration
	// RateLimitRPS and RateLimitBurst set the token bucket for sending messages
	// (per user) and signing in (per client address). Zero in either turns
	// limiting off. Only code that builds Options itself can pass one:
	// config.Load refuses zero for both settings, so a server started from the
	// settings always limits.
	RateLimitRPS   float64
	RateLimitBurst int
	// ReadinessChecks are what GET /readyz looks at, in the order they are
	// reported.
	ReadinessChecks []ReadinessCheck
}

// App owns the shared dependencies of the handlers and builds the route table.
// It is the only place that knows about both HTTP and the use cases; wiring the
// use cases to their adapters happens outside, in internal/app.
type App struct {
	log      *slog.Logger
	renderer *Renderer
	static   *Static
	svc      Services
	opts     Options
	md       *render.Markdown

	sends   *rateLimiter // nil when limiting is off
	reads   *rateLimiter // the pages and the Historial: each is a read of the history, so a user cannot make them without limit
	answers *rateLimiter // the answers to cards: each is a call to the payment service
	logins  *rateLimiter
	ready   readyCache

	// streams is done once the server starts shutting down; see EndStreams.
	streams    context.Context
	endStreams context.CancelFunc
}

// NewApp parses templates and loads static files from files, which must hold
// templates/ and static/ (see package assets).
func NewApp(log *slog.Logger, files fs.FS, svc Services, opts Options) (*App, error) {
	renderer, err := NewRenderer(files)
	if err != nil {
		return nil, err
	}
	static, err := NewStatic(files)
	if err != nil {
		return nil, err
	}
	streams, endStreams := context.WithCancel(context.Background())
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	return &App{
		log: log, renderer: renderer, static: static, svc: svc, opts: opts, md: render.NewMarkdown(),
		sends:   newRateLimiter(opts.RateLimitRPS, opts.RateLimitBurst),
		answers: newRateLimiter(opts.RateLimitRPS, opts.RateLimitBurst),
		logins:  newRateLimiter(opts.RateLimitRPS, opts.RateLimitBurst),
		reads:   newRateLimiter(opts.RateLimitRPS*readsFactor, opts.RateLimitBurst*readsFactor),
		streams: streams, endStreams: endStreams,
	}, nil
}

// EndStreams ends every running reply: each is ended as stopped with the text
// so far, and its browser is told. Streams that start afterwards are sent away
// to try again, so a reply is never stopped before it began. Call it when the
// server starts shutting down.
func (a *App) EndStreams() { a.endStreams() }

// serverError logs err with the request context and answers 500. The client
// never sees the error text.
func (a *App) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.ErrorContext(r.Context(), "server error",
		"request_id", RequestIDFrom(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
	)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// fail answers a request whose use case returned err. Something that is not
// there, or belongs to someone else, is a 404; anything else is a bug or an
// outage, and is logged and answered with a 500.
func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	a.serverError(w, r, err)
}

// fromHTMX reports whether htmx sent the request, and so wants a piece of page to
// swap in. Anything else wants a whole page, or a redirect to one.
func fromHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil
}

// messageRef names a live reply of the signed-in user, as the path of its routes does.
type messageRef struct {
	userID uuid.UUID
	convID string
	msgID  uuid.UUID
}

// messageRefOf reads it from the request, answering 404 itself when the path holds something other than the id of a conversation
// (the form the UI makes) and the id of a reply.
func messageRefOf(w http.ResponseWriter, r *http.Request) (messageRef, bool) {
	convID, err := domain.ParseConversationID(r.PathValue("id"))
	msgID, ok := parseUUID(r.PathValue("mid"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return messageRef{}, false
	}
	user, _ := UserFrom(r.Context())
	return messageRef{userID: user.ID, convID: convID, msgID: msgID}, true
}

// answerWithReply answers a request that put a reply in the place of one: htmx gets the reply to swap in, a plain form post goes back
// to the conversation.
func (a *App) answerWithReply(w http.ResponseWriter, r *http.Request, convID string, reply domain.Message) {
	if !fromHTMX(r) {
		redirect(w, r, "/c/"+convID)
		return
	}
	a.block(w, r, http.StatusOK, "partial:reply", a.messageView(reply))
}

// basePage is embedded by every page's data: it carries what the layout needs.
type basePage struct {
	User *domain.User
}

func (a *App) base(r *http.Request) basePage {
	if u, ok := UserFrom(r.Context()); ok {
		return basePage{User: &u}
	}
	return basePage{}
}

// block renders a template block as the response.
func (a *App) block(w http.ResponseWriter, r *http.Request, status int, name string, data any, extra ...string) {
	out, err := a.renderer.Block(name, data)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	for _, more := range extra {
		out += more
	}
	writeHTML(w, r, status, out)
}

// Package app is the composition root: it builds the real adapters from the
// configuration, connects them to the use cases, and hands back the HTTP
// handler. It is the only place that knows every adapter; the layers it joins
// know only the ports between them.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"iape-ui/assets"
	authcore "iape-ui/internal/core/auth"
	"iape-ui/internal/core/chat"
	"iape-ui/internal/core/conversation"
	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/interaction"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/infrastructure/agent/adkrest"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/infrastructure/api/web"
	authinfra "iape-ui/internal/infrastructure/auth"
	"iape-ui/internal/infrastructure/config"
	"iape-ui/internal/infrastructure/postgres"
)

// App is the assembled application.
type App struct {
	// Handler serves every route, wrapped in the standard middleware.
	Handler http.Handler
	// EndStreams ends the replies being written, saving each as stopped. The
	// server calls it as it starts to shut down.
	EndStreams func()

	pool *pgxpool.Pool
}

// Build connects to the database, runs the startup tasks, and wires the
// application together. On error nothing is left open.
func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	sql, err := postgres.LoadStatements()
	if err != nil {
		return nil, err
	}
	pool, err := postgres.NewPool(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}
	a, err := wire(ctx, cfg, log, pool, sql)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return a, nil
}

func wire(ctx context.Context, cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, sql postgres.Statements) (*App, error) {
	conversations := postgres.NewConversationStore(pool, sql)
	messages := postgres.NewMessageStore(pool, sql)
	users := postgres.NewUserStore(pool, sql)
	sessions := postgres.NewSessionStore(pool, sql)

	// A previous run that crashed leaves replies marked "streaming" that
	// nothing is producing. This process is the only one running (a single
	// instance, see the spec), so everything still streaming is stale.
	recovered, err := messages.RecoverInterrupted(ctx, 0)
	if err != nil {
		return nil, err
	}
	if recovered > 0 {
		log.WarnContext(ctx, "marked interrupted replies as failed", "count", recovered)
	}

	authSvc, err := authcore.NewService(authcore.Dependencies{
		Users:      users,
		Sessions:   sessions,
		Hasher:     authinfra.NewArgon2(),
		Logger:     log,
		SessionTTL: cfg.Session.TTL,
	})
	if err != nil {
		return nil, err
	}
	if cfg.Admin.Email != "" {
		created, err := authSvc.BootstrapAdmin(ctx, cfg.Admin.Email, cfg.Admin.Password.Reveal())
		if err != nil {
			return nil, fmt.Errorf("creating the first user: %w", err)
		}
		if created {
			log.InfoContext(ctx, "created the first user", "email", cfg.Admin.Email)
		}
	}

	agent := newAgent(cfg.Agent)
	actions := newActions(agent)
	chatSvc := chat.NewService(chat.Dependencies{
		Conversations:   conversations,
		Messages:        messages,
		Agent:           agent,
		Logger:          log,
		MaxMessageChars: cfg.Chat.MaxMessageChars,
	})
	interactionSvc := interaction.NewService(interaction.Dependencies{Messages: messages, Actions: actions, Logger: log})
	conversationSvc := conversation.NewService(conversation.Dependencies{
		Conversations: conversations,
		Messages:      messages,
		ListLimit:     cfg.Chat.ListLimit,
	})

	webApp, err := web.NewApp(log, assets.EmbeddedFiles,
		web.Services{Auth: authSvc, Chat: chatSvc, Conversations: conversationSvc, Interactions: interactionSvc},
		web.Options{
			SecureCookies:  cfg.SecureCookies(),
			FlushInterval:  cfg.Chat.StreamFlushInterval,
			RateLimitRPS:   cfg.RateLimit.RPS,
			RateLimitBurst: cfg.RateLimit.Burst,
			ReadinessChecks: []web.ReadinessCheck{
				{Name: "database", Check: pool.Ping},
				{Name: "agent", Check: agent.Ping},
			},
		},
	)
	if err != nil {
		return nil, err
	}
	handler, err := web.Handler(cfg, log, webApp.Routes())
	if err != nil {
		return nil, err
	}
	return &App{Handler: handler, EndStreams: webApp.EndStreams, pool: pool}, nil
}

// Close releases the database connections.
func (a *App) Close() { a.pool.Close() }

// Serve builds the application and serves it on ln until ctx is done, then
// shuts down: it ends the replies being written, lets requests finish, and
// releases the database. It is the program's whole run, so the program and the
// tests that stand in for it go through the same lines.
//
// The caller opens ln first. Building runs the startup recovery, which marks
// every reply still streaming as interrupted; a second process that cannot get
// the port must fail before it can do that to the first one's live replies.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger, ln net.Listener) error {
	application, err := Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()
	return web.NewServer(cfg, log, application.Handler, application.EndStreams).Serve(ctx, ln)
}

// newAgent is the adapter for how the agent is reached: the AG-UI BFF, as a Yape account whose token comes from a program, or
// an ADK REST server (the default).
func newAgent(cfg config.Agent) resource.AgentGateway {
	if cfg.Protocol == config.ProtocolAGUIBFF {
		return aguibff.NewClient(cfg, aguibff.CommandToken{Bin: cfg.BearerBin, AccountID: cfg.AccountID}, nil)
	}
	return adkrest.NewClient(cfg, nil)
}

// newActions answers the cards of the replies. The agent that makes cards (the AG-UI BFF) answers them too; an agent that makes
// none has nothing to answer, and a card it left behind (the protocol was changed) cannot be answered.
func newActions(agent resource.AgentGateway) resource.ActionGateway {
	if actions, ok := agent.(resource.ActionGateway); ok {
		return actions
	}
	return noActions{}
}

// noActions is the ActionGateway of an agent that has no cards.
type noActions struct{}

var _ resource.ActionGateway = noActions{}

func (noActions) Approve(context.Context, domain.Interaction) (domain.ActionResult, error) {
	return domain.ActionResult{}, domain.ErrAgentUnavailable
}

func (noActions) Reject(context.Context, domain.Interaction) (domain.ActionResult, error) {
	return domain.ActionResult{}, domain.ErrAgentUnavailable
}

func (noActions) ConfirmContact(context.Context, string, domain.Contact) error {
	return domain.ErrAgentUnavailable
}

func (noActions) ConfirmOTP(context.Context, domain.Interaction, string) (domain.ActionResult, error) {
	return domain.ActionResult{}, domain.ErrAgentUnavailable
}

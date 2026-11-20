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
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"iape-ui/assets"
	authcore "iape-ui/internal/core/auth"
	"iape-ui/internal/core/chat"
	"iape-ui/internal/core/history"
	"iape-ui/internal/core/interaction"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/infrastructure/api/web"
	authinfra "iape-ui/internal/infrastructure/auth"
	"iape-ui/internal/infrastructure/config"
	"iape-ui/internal/infrastructure/historyapi"
	"iape-ui/internal/infrastructure/memory"
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

// Build connects to the database, makes the user of the Yape account when it is asked to, and wires the
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
	users := postgres.NewUserStore(pool, sql)
	sessions := postgres.NewSessionStore(pool, sql)

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
		account, err := uuid.Parse(cfg.Admin.AccountID) // Load has checked it
		if err != nil {
			return nil, fmt.Errorf("the account of the first user: %w", err)
		}
		created, err := authSvc.BootstrapAdmin(ctx, account, cfg.Admin.Email, cfg.Admin.Password.Reveal())
		if err != nil {
			return nil, fmt.Errorf("creating the user of the Yape account: %w", err)
		}
		if created {
			log.InfoContext(ctx, "created the user of the Yape account", "email", cfg.Admin.Email)
		}
	}

	// The BFF of the Yape agent service is the agent and answers the cards of its replies. Its token comes from a program, once
	// for each request: that program caches it until it expires.
	agent := aguibff.NewClient(cfg.Agent, aguibff.CommandToken{Bin: cfg.Agent.BearerBin}, nil)

	// The conversation history is the only place the chats are kept. What is live here (the replies being written, with their cards,
	// and who started each conversation) is in memory: a restart empties it.
	live := memory.NewLiveStore(memory.Options{})
	var historySvc service.History // nil without a history: the page has no Historial
	var historyGateway resource.HistoryGateway
	if cfg.History.BaseURL != "" {
		historyGateway = historyapi.NewClient(cfg.History, nil)
		historySvc = history.NewService(history.Dependencies{History: historyGateway, Logger: log})
	}
	chatSvc := chat.NewService(chat.Dependencies{
		Live:            live,
		Agent:           agent,
		History:         historyGateway,
		Logger:          log,
		MaxMessageChars: cfg.Chat.MaxMessageChars,
	})
	interactionSvc := interaction.NewService(interaction.Dependencies{Live: live, Actions: agent, Logger: log})

	webApp, err := web.NewApp(log, assets.EmbeddedFiles,
		web.Services{Auth: authSvc, Chat: chatSvc, History: historySvc, Interactions: interactionSvc},
		web.Options{
			SecureCookies:  cfg.SecureCookies(),
			Location:       cfg.History.Location(),
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
// The caller opens ln first: a second process that cannot get the port must fail before it builds anything, or it would make the
// first one's user of the Yape account too.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger, ln net.Listener) error {
	application, err := Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()
	return web.NewServer(cfg, log, application.Handler, application.EndStreams).Serve(ctx, ln)
}

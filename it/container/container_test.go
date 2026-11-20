// The container images, built from their Dockerfiles and run together on one
// network: PostgreSQL, the scripted BFF and the web server, which asks the BFF if it is ready.
// The web server is stopped the way `docker stop` stops it.
//
// Group A: needs Docker, and builds two images (the first build downloads the Go
// modules, so it takes a few minutes; later builds reuse Docker's cache). Skipped
// with -short.
package container_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"iape-ui/it/internal/pgenv"
	"iape-ui/it/internal/stack"
)

const (
	adminEmail    = "container@example.com"
	adminPassword = "a-container-password-1"
)

// root is the repository, the context both images are built from.
func root(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// webImage is the name the web image is built under, so that it can be run again.
const webImage = "agent-web-ui-it-web:latest"

// built builds an image from a Dockerfile of the repository and runs it.
func built(ctx context.Context, t *testing.T, dockerfile string, req testcontainers.ContainerRequest) testcontainers.Container {
	t.Helper()
	repo, tag, _ := strings.Cut(webImage, ":")
	if dockerfile != "Dockerfile" {
		repo, tag = "", "" // any name will do
	}
	req.FromDockerfile = testcontainers.FromDockerfile{Context: root(t), Dockerfile: dockerfile, Repo: repo, Tag: tag}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		if ctr != nil { // it started but did not become ready: say what it said
			t.Logf("output of the %s container:\n%s", dockerfile, logsOf(context.Background(), ctr))
		}
		t.Fatalf("building and starting %s: %v", dockerfile, err)
	}
	return ctr
}

// copyOut copies a file out of a container into a temporary file of the host, and returns its path.
func copyOut(ctx context.Context, t *testing.T, ctr testcontainers.Container, path string) string {
	t.Helper()
	r, err := ctr.CopyFileFromContainer(ctx, path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	out := filepath.Join(t.TempDir(), filepath.Base(path))
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY, 0o755)
	require.NoError(t, err)
	_, err = io.Copy(f, r)
	require.NoError(t, errors.Join(err, f.Close()))
	return out
}

func logsOf(ctx context.Context, ctr testcontainers.Container) string {
	r, err := ctr.Logs(ctx)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = r.Close() }()
	b, _ := io.ReadAll(r)
	return string(b)
}

// runToTheEnd runs the web image with the settings and waits for it to stop.
func runToTheEnd(ctx context.Context, t *testing.T, env map[string]string) (exitCode int, output string) {
	t.Helper()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Image: webImage, Env: env,
		WaitingFor: wait.ForExit().WithExitTimeout(30 * time.Second),
		Started:    true,
	})
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)
	state, err := ctr.State(ctx)
	require.NoError(t, err)
	return state.ExitCode, logsOf(ctx, ctr)
}

func TestContainersSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	nw, err := network.New(ctx)
	require.NoError(t, err)
	testcontainers.CleanupNetwork(t, nw)

	db, err := postgres.Run(ctx, pgenv.Image,
		postgres.WithDatabase("agentwebui"), postgres.WithUsername("agentwebui"), postgres.WithPassword("pg-secret-1"),
		postgres.BasicWaitStrategies(),
		network.WithNetwork([]string{"postgres"}, nw))
	testcontainers.CleanupContainer(t, db)
	require.NoError(t, err)
	// The program creates no table: it expects its schema, as the migrations leave it.
	hostURL, err := db.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pgenv.ApplySchema(t, hostURL)

	agent := built(ctx, t, "Dockerfile.testbff", testcontainers.ContainerRequest{
		Networks:       []string{nw.Name},
		NetworkAliases: map[string][]string{nw.Name: {"agent"}},
		// Distroless has no shell or curl: the binary asks itself, as Compose does.
		WaitingFor: wait.ForExec([]string{"/test-bff", "healthcheck"}).WithStartupTimeout(2 * time.Minute),
	})
	// A chat needs the program that prints a token (yape-token), which belongs to the computer the Web UI runs on. The scripted BFF
	// is such a program too (test-bff --account ID), so its binary is copied into the web image.
	tokenBin := copyOut(ctx, t, agent, "/test-bff")
	web := built(ctx, t, "Dockerfile", testcontainers.ContainerRequest{
		Files:          []testcontainers.ContainerFile{{HostFilePath: tokenBin, ContainerFilePath: "/yape-token", FileMode: 0o755}},
		Networks:       []string{nw.Name},
		NetworkAliases: map[string][]string{nw.Name: {"web"}},
		ExposedPorts:   []string{"8300/tcp"},
		Env: map[string]string{
			"APP_ENV":            "development",
			"APP_BASE_URL":       "http://localhost:8300",
			"DB_URL":             "postgres://agentwebui:pg-secret-1@postgres:5432/agentwebui?sslmode=disable&search_path=web_ui",
			"AGENT_BASE_URL":     "http://agent:9093",
			"YAPE_ACCOUNT_ID":    "25906ac5-9f18-4099-b3fd-e1b74d8549a4",
			"YAPE_TOKEN_BIN":     "/yape-token",
			"ADMIN_EMAIL":        adminEmail,
			"ADMIN_PASSWORD":     adminPassword,
			"APP_SHUTDOWN_GRACE": "10s",
		},
		// /readyz asks the database and the agent, over the network between the containers.
		WaitingFor: wait.ForHTTP("/readyz").WithPort("8300/tcp").WithStartupTimeout(2 * time.Minute),
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("web server log:\n%s\nagent log:\n%s", logsOf(context.Background(), web), logsOf(context.Background(), agent))
		}
	})

	t.Run("the images run as an ordinary user, not root", func(t *testing.T) {
		for name, ctr := range map[string]testcontainers.Container{"web": web, "agent": agent} {
			info, err := ctr.Inspect(ctx)
			require.NoError(t, err)
			assert.NotContains(t, []string{"", "root", "0", "0:0"}, info.Config.User, "%s runs as %q", name, info.Config.User)
		}
	})

	t.Run("their health checks answer", func(t *testing.T) {
		for _, probe := range []struct {
			name string
			ctr  testcontainers.Container
			cmd  []string
		}{
			{"web", web, []string{"/agent-web-ui", "healthcheck"}},
			{"agent", agent, []string{"/test-bff", "healthcheck"}},
		} {
			code, out, err := probe.ctr.Exec(ctx, probe.cmd)
			require.NoError(t, err)
			output, _ := io.ReadAll(out)
			assert.Zero(t, code, "%s: %s", probe.name, output)
		}
	})

	t.Run("a chat goes through the web server, the scripted BFF and the database", func(t *testing.T) {
		host, err := web.Host(ctx)
		require.NoError(t, err)
		port, err := web.MappedPort(ctx, "8300/tcp")
		require.NoError(t, err)
		c := stack.NewClient(t, "http://"+host+":"+port.Port()).SignIn(adminEmail, adminPassword)

		conv, reply := c.Start("hello from the containers")
		stream, cancelStream := context.WithTimeout(ctx, 30*time.Second)
		defer cancelStream()
		events := c.OpenStream(stream, conv, reply).Drain()

		require.NotEmpty(t, events)
		assert.Equal(t, "done", events[len(events)-1].Name)
		final := stack.MessagesIn(t, stack.Named(events, "delta")[len(stack.Named(events, "delta"))-1].Data)[0]
		assert.Equal(t, "You said: hello from the containers", stack.RawOf(final))
	})

	t.Run("the web server signs in and serves the start page and its assets", func(t *testing.T) {
		host, err := web.Host(ctx)
		require.NoError(t, err)
		port, err := web.MappedPort(ctx, "8300/tcp")
		require.NoError(t, err)
		c := stack.NewClient(t, "http://"+host+":"+port.Port()).SignIn(adminEmail, adminPassword)

		start := c.Get("/")

		assert.Equal(t, 200, start.Status)
		assert.Contains(t, start.Body, "composer-input", "the page has its composer")
		css := c.Get("/static/css/app.css")
		assert.Equal(t, 200, css.Status, "the assets embedded in the binary are served")
		assert.Contains(t, css.Header.Get("Content-Type"), "text/css")
	})

	t.Run("a web server without its settings stops, and says which are missing (SC-15)", func(t *testing.T) {
		code, output := runToTheEnd(ctx, t, nil)

		assert.NotZero(t, code)
		assert.Contains(t, output, "missing required configuration")
		for _, key := range []string{"DB_URL", "AGENT_BASE_URL", "YAPE_ACCOUNT_ID"} {
			assert.Contains(t, output, key)
		}
	})

	t.Run("a web server with settings that make no sense stops, and says what is wrong (SC-15)", func(t *testing.T) {
		code, output := runToTheEnd(ctx, t, map[string]string{
			"DB_URL": "postgres://u:p@postgres:5432/db", "AGENT_BASE_URL": "not-a-url", "YAPE_ACCOUNT_ID": "25906ac5-9f18-4099-b3fd-e1b74d8549a4",
			"ADMIN_EMAIL": "not-an-email", "ADMIN_PASSWORD": "short",
		})

		assert.NotZero(t, code)
		assert.Contains(t, output, "AGENT_BASE_URL must be an absolute http or https URL")
		assert.Contains(t, output, "ADMIN_EMAIL must be a plain email address")
		assert.Contains(t, output, "ADMIN_PASSWORD must be at least 12 characters")
		assert.NotContains(t, output, "short", "and a password is never repeated")
	})

	// The program's own wiring, in its own image: a reply being written when the
	// signal comes is ended and saved as stopped, and the server is gone at once.
	// Without that, it would wait out its grace period for a stream that runs for
	// as long as the reply does.
	t.Run("docker stop ends a reply being written, saves it, and exits with code 0", func(t *testing.T) {
		host, err := web.Host(ctx)
		require.NoError(t, err)
		port, err := web.MappedPort(ctx, "8300/tcp")
		require.NoError(t, err)
		c := stack.NewClient(t, "http://"+host+":"+port.Port()).SignIn(adminEmail, adminPassword)
		conv, reply := c.Start("go on and on #slow")
		stream := c.OpenStream(ctx, conv, reply)
		defer stream.Close()
		for ev, ok := stream.Next(); ok; ev, ok = stream.Next() {
			if ev.Name == "delta" && strings.Contains(ev.Data, "tick") {
				break // some of the reply is on the page
			}
		}
		timeout := 20 * time.Second
		started := time.Now()

		require.NoError(t, web.Stop(ctx, &timeout))

		assert.Less(t, time.Since(started), 5*time.Second, "SIGTERM reaches the program: it is PID 1 of its container")
		state, err := web.State(ctx)
		require.NoError(t, err)
		assert.Zero(t, state.ExitCode)
		dsn, err := db.ConnectionString(ctx, "sslmode=disable")
		require.NoError(t, err)
		conn, err := pgx.Connect(ctx, dsn)
		require.NoError(t, err)
		defer func() { _ = conn.Close(ctx) }()
		var status, content string
		require.NoError(t, conn.QueryRow(ctx, `SELECT status, content FROM web_ui.messages WHERE id = $1`, reply).Scan(&status, &content))
		assert.Equal(t, "stopped", status, "the reply was ended by the shutdown, not left streaming or cut by the exit")
		assert.Contains(t, content, "tick", "and the text so far was saved")
	})
}

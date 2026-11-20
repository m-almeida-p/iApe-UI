// Command test-bff runs the scripted stand-in for the agent service's BFF (internal/testbff): the AG-UI endpoint the Web UI talks to,
// answering from a script. Start it, and point the Web UI at it with AGENT_BASE_URL=http://localhost:9093:
//
//	test-bff                       listens on :9093 (TEST_BFF_ADDR)
//	test-bff healthcheck [url]     exits 0 when it answers, for container health checks in images that have no curl
//	test-bff --account ID          prints a token the scripted BFF accepts, as yape-token does: this binary can be the Web UI's
//	                               YAPE_TOKEN_BIN where no other program is at hand (the container test copies it into the image)
//
// The pace of its replies can be changed in .env (written down in .env.example; a variable in the environment wins, as it does in
// the container), for tests that need a long reply:
//
//	TEST_BFF_ADDR          where it listens (default :9093; the image listens on every interface, Compose publishes 127.0.0.1)
//	TEST_BFF_WORD_DELAY    pause between words of ordinary replies (default 10ms)
//	TEST_BFF_SLOW_DELAY    pause between the chunks of a #slow reply (default 100ms)
//	TEST_BFF_SLOW_CHUNKS   how many chunks a #slow reply has (default 50)
//
// The text of a message says what the reply is (#slow, #error, #tool, #markdown, #html, and the cards #pay, #otp, #contact): see
// internal/testbff.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"iape-ui/internal/platform/envcfg"
	"iape-ui/internal/testbff"
)

const defaultAddr = ":9093"

// maxSlowChunks is the longest #slow reply that can be asked for: its chunks are made before the first one is sent, so a number that
// is a typo of a few digits more would eat the memory of the container before it said anything.
const maxSlowChunks = 1_000_000

// healthTimeout is how long healthcheck waits for an answer.
var healthTimeout = 3 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "--account" { // the way the Web UI asks yape-token for a token
		fmt.Println("test-token")
		return
	}
	lookup, err := envcfg.EnvLookup(".env")
	if err != nil {
		log.Fatalf("test-bff: %v", err)
	}
	opts, err := options(lookup)
	if err != nil {
		log.Fatalf("test-bff: %v", err)
	}
	addr := defaultAddr
	if v, ok := lookup("TEST_BFF_ADDR"); ok && v != "" {
		addr = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: testbff.NewHandler(opts...), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("test-bff: listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("test-bff: %v", err)
	}
}

// healthcheck asks for /health.
func healthcheck(args []string) int {
	url := "http://127.0.0.1:9093/health"
	if len(args) > 0 {
		url = args[0]
	}
	resp, err := (&http.Client{Timeout: healthTimeout}).Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "unhealthy: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// options reads the pacing settings. A value that is set must be valid, so a typo does not quietly leave a test with a reply of the
// wrong length.
func options(lookup func(string) (string, bool)) ([]testbff.Option, error) {
	duration := func(key string, fallback time.Duration) (time.Duration, error) {
		raw, ok := lookup(key)
		if !ok || raw == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("%s=%q is not a duration such as 100ms", key, raw)
		}
		return d, nil
	}
	word, err := duration("TEST_BFF_WORD_DELAY", testbff.DefaultWordDelay)
	if err != nil {
		return nil, err
	}
	slow, err := duration("TEST_BFF_SLOW_DELAY", testbff.DefaultSlowDelay)
	if err != nil {
		return nil, err
	}
	chunks := testbff.DefaultSlowChunks
	if raw, ok := lookup("TEST_BFF_SLOW_CHUNKS"); ok && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxSlowChunks {
			return nil, fmt.Errorf("TEST_BFF_SLOW_CHUNKS=%q is not a number from 1 to %d", raw, maxSlowChunks)
		}
		chunks = n
	}
	return []testbff.Option{testbff.WithWordDelay(word), testbff.WithSlow(slow, chunks)}, nil
}

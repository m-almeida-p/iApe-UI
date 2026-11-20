//go:build unix

package chrome_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

func init() {
	extraTools["hang"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		time.Sleep(time.Hour)
		return textResult("never"), nil
	}
	extraTools["noisy_crash"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		for range 2000 {
			fmt.Fprintln(os.Stderr, "debug: still starting the browser, "+strings.Repeat("é", 100))
		}
		fmt.Fprintln(os.Stderr, "fatal: the end of the noise")
		os.Exit(9)
		return nil, nil
	}
}

// pidIn finds the pid the fake server printed on its standard error in an error message.
func pidIn(t *testing.T, err error) int {
	t.Helper()
	_, after, found := strings.Cut(err.Error(), "pid=")
	require.True(t, found, "the server printed its pid: %v", err)
	pid, convErr := strconv.Atoi(strings.TrimSpace(strings.SplitN(after, " ", 2)[0]))
	require.NoError(t, convErr)
	return pid
}

func TestKillTree_EdgeCases(t *testing.T) {
	t.Run("a process that has already exited and been reaped", func(t *testing.T) {
		cmd := exec.Command("true")
		chrome.Isolate(cmd)
		require.NoError(t, cmd.Run())

		assert.NotPanics(t, func() { chrome.KillTree(cmd) })
	})
	t.Run("a process that was never given a group of its own is still killed", func(t *testing.T) {
		cmd := exec.Command("sleep", "300")
		require.NoError(t, cmd.Start())
		pid := cmd.Process.Pid

		chrome.KillTree(cmd)

		_ = cmd.Wait()
		waitGone(t, pid)
	})
	t.Run("killing twice", func(t *testing.T) {
		cmd := exec.Command("sleep", "300")
		chrome.Isolate(cmd)
		require.NoError(t, cmd.Start())

		chrome.KillTree(cmd)
		_ = cmd.Wait()

		assert.NotPanics(t, func() { chrome.KillTree(cmd) })
	})
	t.Run("a command with no process", func(t *testing.T) {
		assert.NotPanics(t, func() { chrome.KillTree(&exec.Cmd{}) })
	})
}

func TestIsolate_GivesTheProcessAGroupOfItsOwn(t *testing.T) {
	cmd := exec.Command("true")

	chrome.Isolate(cmd)
	chrome.Isolate(cmd) // twice is the same

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.Setpgid)
}

func TestBuildCommand_CancelingTheContextKillsTheWholeGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd, err := chrome.BuildCommand(ctx, []string{fakeShellNpx(t), "-y", "chrome-devtools-mcp@1.10.1"}, privacyFlags, []string{"PATH=" + os.Getenv("PATH")}, false)
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var line [32]byte
	n, err := out.Read(line[:])
	require.NoError(t, err)
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	require.NoError(t, err)
	require.True(t, processAlive(child))

	cancel()

	_ = cmd.Wait()
	waitGone(t, child)
	waitGone(t, cmd.Process.Pid)
}

// fakeShellNpx is a program named npx that starts a child of its own, prints its pid and waits: a server and its browser.
func fakeShellNpx(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/npx"
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nsleep 300 &\necho $!\nwait\n"), 0o700))
	return path
}

func TestOpen_EdgeCases(t *testing.T) {
	t.Run("no command at all", func(t *testing.T) {
		for _, command := range [][]string{nil, {}, {""}} {
			s, err := chrome.Open(t.Context(), chrome.Options{Command: command, Args: DefaultArgsForTest()})

			require.Error(t, err)
			assert.Nil(t, s)
			assert.ErrorContains(t, err, "must pin a version")
		}
	})
	t.Run("an environment of nothing the server may have is not this program's", func(t *testing.T) {
		opts := processOptions(t)
		opts.Environ = []string{"HF_TOKEN=CANARY", "DB_URL=postgres://u:CANARY@h/d"}
		s, err := chrome.Open(t.Context(), opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close(context.Background()) })

		got, err := s.Call(t.Context(), "env", nil)

		require.NoError(t, err)
		assert.NotContains(t, got.Text, "HF_TOKEN")
		assert.NotContains(t, got.Text, "DB_URL")
		assert.NotContains(t, got.Text, "CANARY")
	})
	t.Run("an environment given as empty is empty, not this process's", func(t *testing.T) {
		t.Setenv("HF_TOKEN", "CANARY")
		opts := processOptions(t)
		opts.Environ = []string{}
		s, err := chrome.Open(t.Context(), opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close(context.Background()) })

		got, err := s.Call(t.Context(), "env", nil)

		require.NoError(t, err)
		assert.NotContains(t, got.Text, "HF_TOKEN")
	})
	t.Run("a server that prints garbage instead of the handshake is given up on and stopped", func(t *testing.T) {
		opts := processOptions(t, "garbage")
		opts.StartTimeout = 500 * time.Millisecond
		start := time.Now()

		_, err := chrome.Open(t.Context(), opts)

		require.Error(t, err)
		assert.Less(t, time.Since(start), 4*time.Second)
		assert.ErrorContains(t, err, "starting Chrome DevTools MCP")
		waitGone(t, pidIn(t, err))
	})
	t.Run("a start context that is already canceled starts nothing that stays", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		s, err := chrome.Open(ctx, processOptions(t, "silent"))

		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, s)
		if strings.Contains(err.Error(), "pid=") {
			waitGone(t, pidIn(t, err))
		}
	})
	t.Run("a start context that ends before the start time does", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		opts := processOptions(t, "silent")
		opts.StartTimeout = time.Hour
		start := time.Now()

		_, err := chrome.Open(ctx, opts)

		require.Error(t, err)
		assert.Less(t, time.Since(start), 4*time.Second)
		waitGone(t, pidIn(t, err))
	})
	t.Run("a program that is not executable", func(t *testing.T) {
		path := t.TempDir() + "/npx"
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600))
		opts := processOptions(t)
		opts.Command = []string{path, "-y", "chrome-devtools-mcp@1.10.1"}

		s, err := chrome.Open(t.Context(), opts)

		require.ErrorContains(t, err, "starting Chrome DevTools MCP")
		assert.Nil(t, s)
	})
	t.Run("a program that exits at once with nothing to say", func(t *testing.T) {
		path := t.TempDir() + "/npx"
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700))
		opts := processOptions(t)
		opts.Command = []string{path, "-y", "chrome-devtools-mcp@1.10.1"}

		s, err := chrome.Open(t.Context(), opts)

		require.ErrorContains(t, err, "starting Chrome DevTools MCP")
		assert.NotContains(t, err.Error(), "its output ended with")
		assert.Nil(t, s)
	})
}

// DefaultArgsForTest are the flags a good run has.
func DefaultArgsForTest() []string {
	return []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}
}

func TestOpen_AServerThatDiesNoisily(t *testing.T) {
	s, err := chrome.Open(t.Context(), processOptions(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	_, err = s.Call(t.Context(), "noisy_crash", nil)

	require.ErrorIs(t, err, chrome.ErrDriverDied)
	assert.ErrorContains(t, err, "its output ended with: …")
	assert.ErrorContains(t, err, "fatal: the end of the noise", "the end of the output is the part that is kept")
	assert.Less(t, utf8.RuneCountInString(err.Error()), 1_000, "a message does not carry the output of a server that talked for a megabyte")
	assert.True(t, utf8.ValidString(err.Error()))
}

func TestOpen_CloseWhileACallIsInFlight(t *testing.T) {
	// A server in the middle of a call does not leave when its input closes; Close does not wait for it for ever.
	s, err := chrome.Open(t.Context(), processOptions(t))
	require.NoError(t, err)
	pid := callPID(t, s)
	called := make(chan error, 1)
	go func() {
		_, err := s.Call(t.Context(), "hang", nil)
		called <- err
	}()
	time.Sleep(200 * time.Millisecond) // the call is in the server's hands
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err = s.Close(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case err := <-called:
		require.ErrorIs(t, err, chrome.ErrDriverDied)
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not return after the server was killed")
	}
	waitGone(t, pid, 5*time.Second)
}

func TestOpen_ASessionThatIsGoneRefusesEverything(t *testing.T) {
	s, err := chrome.Open(t.Context(), processOptions(t))
	require.NoError(t, err)
	pid := callPID(t, s)
	require.NoError(t, syscall.Kill(pid, syscall.SIGKILL), "the browser is killed from outside")

	_, err = s.Call(t.Context(), "echo", nil)

	require.ErrorIs(t, err, chrome.ErrDriverDied)
	_, err = s.Call(t.Context(), "echo", nil)
	require.ErrorIs(t, err, chrome.ErrDriverDied)
	require.NoError(t, s.Close(t.Context()), "closing what is already gone is not an error")
}

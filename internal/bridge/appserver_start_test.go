package bridge

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A subprocess fixture implements the actual Unix/WebSocket initialization
// handshake, and remains alive after the launcher has connected.
func TestAppServerLaunchProcess(t *testing.T) {
	socket := os.Getenv("CSC_TEST_APP_SOCKET")
	if socket == "" {
		t.Skip("app-server subprocess fixture")
	}
	if os.Getenv("CSC_TEST_IGNORE_TERM") == "1" {
		signal.Ignore(unix.SIGTERM)
	}
	// Match Codex's stale-listener recovery after a terminated server.
	if st, err := os.Lstat(socket); err == nil && st.Mode().Type() == os.ModeSocket {
		must(t, os.Remove(socket))
	}
	fake := newFakeAppAt(t, socket)
	if status := os.Getenv("CSC_TEST_APP_STATUS"); status != "" {
		fake.mu.Lock()
		fake.status = status
		fake.mu.Unlock()
	}
	select {}
}

func TestAppServerStartupReusesExistingServer(t *testing.T) {
	fake := newFakeApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := ensureAppServer(ctx, "/nonexistent/codex", fake.path, "/")
	must(t, err)
	client.Close()
}

func TestAppServerStartupSerializesConcurrentLaunchers(t *testing.T) {
	dir := testDir(t)
	socket := filepath.Join(dir, "app.sock")
	pidPath := filepath.Join(dir, "server.pid")
	argsPath := filepath.Join(dir, "args")
	codex := filepath.Join(dir, "codex")
	exe, err := os.Executable()
	must(t, err)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellQuote(argsPath) + "\n" +
		"if [ \"$1\" != app-server ] || [ \"$2\" != --listen ]; then exit 23; fi\n" +
		"printf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "\n" +
		"export CSC_TEST_APP_SOCKET=\"${3#unix://}\"\nexec " + shellQuote(exe) + " '-test.run=^TestAppServerLaunchProcess$'\n"
	must(t, os.WriteFile(codex, []byte(script), 0700))
	t.Cleanup(func() { stopAppServerFixture(t, pidPath) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			client, err := ensureAppServer(ctx, codex, socket, dir)
			if err == nil {
				client.Close()
			}
			results <- err
		}()
	}
	for range 2 {
		must(t, <-results)
	}
	b, err := os.ReadFile(argsPath)
	must(t, err)
	if string(b) != "app-server\n--listen\nunix://"+socket+"\n" {
		t.Fatalf("expected one direct server start, got %s", b)
	}
	// The server remains available after both startup contexts and clients close.
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialApp(ctx, socket)
	must(t, err)
	client.Close()
}

func stopAppServerFixture(t *testing.T, pidPath string) {
	t.Helper()
	b, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	must(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	must(t, err)
	if pid <= 0 {
		t.Fatalf("invalid fixture PID: %d", pid)
	}
	_ = unix.Kill(pid, unix.SIGKILL)
}

func TestAppServerStartupFailureAndTimeout(t *testing.T) {
	for _, mode := range []string{"exit", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := testDir(t)
			codex := filepath.Join(dir, "codex")
			pidPath := filepath.Join(dir, "server.pid")
			script := "#!/bin/sh\nprintf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "\n"
			if mode == "exit" {
				script += "echo startup-failed >&2\nexit 23\n"
			} else {
				script += "exec sleep 30\n"
			}
			must(t, os.WriteFile(codex, []byte(script), 0700))
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := ensureAppServer(ctx, codex, filepath.Join(dir, "app.sock"), dir)
			if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "cross-session-codex.log")) {
				t.Fatalf("expected startup failure with log path, got %v", err)
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected startup deadline, got %v", err)
			}
			if mode == "exit" {
				b, readErr := os.ReadFile(filepath.Join(dir, "cross-session-codex.log"))
				must(t, readErr)
				if !strings.Contains(string(b), "startup-failed") {
					t.Fatalf("server stderr missing: %s", b)
				}
			}
			b, err := os.ReadFile(pidPath)
			must(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			must(t, err)
			if err = unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
				t.Fatalf("failed startup left a running process: %v", err)
			}
		})
	}
}

func TestAppServerStartupRefusesUnsafeSocket(t *testing.T) {
	dir := testDir(t)
	socket := filepath.Join(dir, "app.sock")
	must(t, os.WriteFile(socket, []byte("unrelated"), 0600))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := ensureAppServer(ctx, "/nonexistent/codex", socket, dir)
	if err == nil || !strings.Contains(err.Error(), "unsafe type") {
		t.Fatalf("expected refusal of unrelated socket path, got %v", err)
	}
	b, err := os.ReadFile(socket)
	must(t, err)
	if string(b) != "unrelated" {
		t.Fatal("replaced unrelated file")
	}
}

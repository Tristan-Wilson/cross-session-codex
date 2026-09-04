package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func shutdownFixture(t *testing.T) (string, string, *appClient) {
	t.Helper()
	dir := isolatedState(t)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	socket := defaultAppSocket()
	pidPath := filepath.Join(dir, "server.pid")
	codex := filepath.Join(dir, "fixture-codex")
	exe, err := os.Executable()
	must(t, err)
	script := "#!/bin/sh\nprintf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "\n" +
		"export CSC_TEST_APP_SOCKET=\"${3#unix://}\"\nexec " + shellQuote(exe) + " '-test.run=^TestAppServerLaunchProcess$'\n"
	must(t, os.WriteFile(codex, []byte(script), 0700))
	t.Cleanup(func() { stopAppServerFixture(t, pidPath) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := ensureAppServer(ctx, codex, socket, dir)
	must(t, err)
	t.Cleanup(client.Close)
	return socket, codex, client
}

func TestShutdownRefusesConnectionsThenStopsAndRestarts(t *testing.T) {
	socket, codex, client := shutdownFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info := cli(t, "shutdown", "--check")
	if str(info, "status") != "busy" || number(info, "connections", 0) != 1 {
		t.Fatalf("failed to detect an unregistered client: %v", info)
	}
	_, err := Shutdown(ctx, socket, false)
	if err == nil || !strings.Contains(err.Error(), "connected client") {
		t.Fatalf("shutdown did not refuse connected client: %v", err)
	}
	if client.Closed() {
		t.Fatal("refused shutdown closed the existing client")
	}
	client.Close()
	eventually(t, func() bool {
		info, err := Shutdown(ctx, socket, true)
		return err == nil && str(info, "status") == "ready"
	})
	info = cli(t, "shutdown")
	if str(info, "status") != "stopped" {
		t.Fatalf("shutdown failed: %v", info)
	}
	info = cli(t, "shutdown")
	if str(info, "status") != "not_running" {
		t.Fatalf("shutdown was not idempotent: %v", info)
	}
	// Startup must recover the stopped server's socket and replace its PID record.
	client, err = ensureAppServer(ctx, codex, socket, filepath.Dir(codex))
	must(t, err)
	client.Close()
	info, err = Shutdown(ctx, socket, false)
	must(t, err)
	if str(info, "status") != "stopped" {
		t.Fatalf("restarted server could not stop: %v", info)
	}
}

func TestShutdownRefusesOwnerAfterMessagingDisabled(t *testing.T) {
	socket, _, client := shutdownFixture(t)
	ctx := context.Background()
	var response struct{ Thread appThread }
	must(t, client.call(ctx, "thread/start", Object{}, &response))
	start, err := processStart(os.Getpid())
	must(t, err)
	_, err = Enable(response.Thread.ID, Object{"name": "open-window", "app_server_socket": socket, "host_pid": os.Getpid(), "host_start": start})
	must(t, err)
	t.Cleanup(func() { _ = Stop(response.Thread.ID) })
	must(t, Stop(response.Thread.ID))
	client.Close()
	info, err := Shutdown(ctx, socket, true)
	must(t, err)
	if str(info, "status") != "busy" || len(info["sessions"].([]Object)) != 1 {
		t.Fatalf("disabled messaging hid its live owner: %v", info)
	}
	_, err = Shutdown(ctx, socket, false)
	if err == nil || !strings.Contains(err.Error(), "1 live session owner") {
		t.Fatalf("did not refuse live owner: %v", err)
	}
}

func TestShutdownRefusesActiveThreads(t *testing.T) {
	t.Setenv("CSC_TEST_APP_STATUS", "active")
	socket, _, client := shutdownFixture(t)
	client.Close()
	info, err := Shutdown(context.Background(), socket, true)
	must(t, err)
	if str(info, "status") != "busy" || len(info["active_threads"].([]string)) != 1 {
		t.Fatalf("missed active background thread: %v", info)
	}
	_, err = Shutdown(context.Background(), socket, false)
	if err == nil || !strings.Contains(err.Error(), "1 active thread") {
		t.Fatalf("did not refuse active thread: %v", err)
	}
}

func TestShutdownRefusesUnverifiedServerIdentity(t *testing.T) {
	socket, _, client := shutdownFixture(t)
	defer client.Close()
	path := appServerRecordPath(socket)
	var recorded appServerRecord
	must(t, readJSON(path, true, &recorded))
	recorded.Start = "different process start"
	must(t, atomicJSON(path, recorded, 0600))
	_, err := Shutdown(context.Background(), socket, false)
	if err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("accepted stale process identity: %v", err)
	}
	must(t, os.Remove(path))
	_, err = Shutdown(context.Background(), socket, false)
	if err == nil || !strings.Contains(err.Error(), "no matching bridge launch record") {
		t.Fatalf("accepted unrelated server: %v", err)
	}
	if client.Closed() {
		t.Fatal("identity refusal stopped the server")
	}
}

func TestShutdownAllowsCompletedErrorThread(t *testing.T) {
	// Codex reports systemError only after clearing its running-turn state.
	t.Setenv("CSC_TEST_APP_STATUS", "systemError")
	socket, _, client := shutdownFixture(t)
	client.Close()
	info, err := Shutdown(context.Background(), socket, false)
	must(t, err)
	if str(info, "status") != "stopped" {
		t.Fatalf("completed error thread prevented shutdown: %v", info)
	}
}

func TestShutdownTimeoutDoesNotForceKill(t *testing.T) {
	t.Setenv("CSC_TEST_IGNORE_TERM", "1")
	socket, _, client := shutdownFixture(t)
	pid := client.serverPID
	client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err := Shutdown(ctx, socket, false)
	if err == nil || !strings.Contains(err.Error(), "no force kill") {
		t.Fatalf("unexpected shutdown timeout: %v", err)
	}
	if err = unix.Kill(pid, 0); err != nil {
		t.Fatalf("shutdown killed an unresponsive server: %v", err)
	}
}

func TestShutdownSerializesWithLaunch(t *testing.T) {
	dir := isolatedState(t)
	socket := filepath.Join(dir, "app.sock")
	lock, err := lockAppServer(context.Background(), socket)
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = Shutdown(ctx, socket, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown ignored launch lock: %v", err)
	}
	closeQuietly(lock)
	info, err := Shutdown(context.Background(), socket, false)
	must(t, err)
	if str(info, "status") != "not_running" {
		t.Fatalf("shutdown did not recover after lock released: %v", info)
	}
}

func TestShutdownCLIRefusesInsideSession(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", uuid.NewString())
	var out bytes.Buffer
	code := Main([]string{"shutdown"}, strings.NewReader(""), &out, &out)
	if code != 1 || !strings.Contains(out.String(), "after exiting all Codex sessions") {
		t.Fatalf("shutdown allowed from inside a session: %d %s", code, out.String())
	}
}

func TestShutdownOwnerPIDReuseAndSocketScope(t *testing.T) {
	dir := isolatedState(t)
	thread := uuid.NewString()
	state, err := threadDir(thread)
	must(t, err)
	must(t, privateDir(state))
	socket := filepath.Join(dir, "app.sock")
	start, err := processStart(os.Getpid())
	must(t, err)
	for _, cfg := range []Config{
		{HostPID: os.Getpid(), HostStart: "stale start", AppSocket: socket},
		{HostPID: os.Getpid(), HostStart: start, AppSocket: socket + ".other"},
	} {
		must(t, atomicJSON(filepath.Join(state, "config.json"), cfg, 0600))
		owners, err := appServerOwners(socket)
		must(t, err)
		if len(owners) != 0 {
			t.Fatalf("unrelated or reused PID blocked shutdown: %v", owners)
		}
	}
}

func TestCountAppConnections(t *testing.T) {
	for _, suffix := range []string{"", " type=STREAM (CONNECTED)"} {
		data := fmt.Sprintf("p123\x00\nf7\x00tunix\x00n/tmp/app.sock%s\x00\nf8\x00tunix\x00n/tmp/app.sock%s\x00\nf9\x00tunix\x00n/tmp/app.sock%s\x00\nf10\x00tunix\x00n/tmp/app.sock.other\x00\n", suffix, suffix, suffix)
		got, err := countAppConnections([]byte(data), 123, "/tmp/app.sock")
		must(t, err)
		if got != 1 {
			t.Fatalf("wrong connection count: %d", got)
		}
		if _, err = countAppConnections([]byte(data), 124, "/tmp/app.sock"); err == nil {
			t.Fatal("accepted output for a different process")
		}
	}
}

func TestShutdownRefusesMissingConnectionInspector(t *testing.T) {
	// Check the real exec boundary, not a fabricated zero-client result.
	t.Setenv("PATH", testDir(t))
	_, err := appServerConnections(context.Background(), os.Getpid(), "/tmp/app.sock")
	if err == nil || !strings.Contains(err.Error(), "requires lsof") {
		t.Fatalf("missing inspector did not fail closed: %v", err)
	}
}

func TestShutdownRefusesUnsafeSocket(t *testing.T) {
	dir := isolatedState(t)
	socket := filepath.Join(dir, "app.sock")
	must(t, os.WriteFile(socket, []byte("unrelated file"), 0600))
	_, err := Shutdown(context.Background(), socket, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe type") {
		t.Fatalf("accepted non-socket path: %v", err)
	}
}

func TestShutdownDoesNotReportLiveServerAsStoppedWithoutSocket(t *testing.T) {
	dir := isolatedState(t)
	socket := filepath.Join(dir, "app.sock")
	start, err := processStart(os.Getpid())
	must(t, err)
	must(t, atomicJSON(appServerRecordPath(socket), appServerRecord{PID: os.Getpid(), Start: start, Socket: socket}, 0600))
	_, err = Shutdown(context.Background(), socket, false)
	if err == nil || !strings.Contains(err.Error(), "may still be alive") {
		t.Fatalf("reported an alive process as stopped: %v", err)
	}
}

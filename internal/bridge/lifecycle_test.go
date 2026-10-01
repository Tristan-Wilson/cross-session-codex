package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConcurrentNamesAndRenameCollision(t *testing.T) {
	isolatedState(t)
	exe, err := os.Executable()
	must(t, err)
	type attempt struct {
		thread string
		output []byte
		err    error
	}
	results := make(chan attempt, 2)
	start := make(chan struct{})
	for range 2 {
		thread := uuid.NewString()
		go func() {
			<-start
			output, err := exec.Command(exe, "start", "--thread", thread, "--name", "unique-peer", "--delivery", "manual").CombinedOutput()
			results <- attempt{thread, output, err}
		}()
	}
	close(start)
	var winner string
	for range 2 {
		r := <-results
		if r.err == nil {
			if winner != "" {
				t.Fatal("both workers claimed the same name")
			}
			winner = r.thread
			t.Cleanup(func() { _ = Stop(r.thread) })
		} else if !strings.Contains(string(r.output), "already registered") {
			t.Fatalf("unexpected startup error: %v %s", r.err, r.output)
		}
	}
	if winner == "" {
		t.Fatal("neither worker registered")
	}
	other := uuid.NewString()
	startTestPeer(t, other, "other-peer")
	if _, err = RPC(other, "configure", Object{"name": " unique-peer "}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("rename should reject a normalized duplicate: %v", err)
	}
	info, err := RPC(other, "info", nil)
	must(t, err)
	if str(info, "name") != "other-peer" {
		t.Fatal("failed rename changed the running name")
	}
	dir, err := threadDir(other)
	must(t, err)
	var cfg Config
	must(t, readJSON(filepath.Join(dir, "config.json"), true, &cfg))
	if cfg.Name != "other-peer" {
		t.Fatal("failed rename changed the persisted name")
	}
	cli(t, "start", "--thread", winner, "--name", "unique-peer")
	must(t, Stop(winner))
	_, err = RPC(other, "configure", Object{"name": "unique-peer"})
	must(t, err)
}

func TestUnownedStartsAndHooksCannotCreateAutomaticWorkers(t *testing.T) {
	isolatedState(t)
	fake := newFakeApp(t)
	_, err := Enable(fake.thread, Object{"name": "unowned", "app_server_socket": fake.path})
	if err == nil || !strings.Contains(err.Error(), "client owner") {
		t.Fatalf("unowned automatic start should fail: %v", err)
	}
	result, err := handleHook(Object{"session_id": fake.thread, "hook_event_name": "SessionStart"})
	must(t, err)
	if len(result) != 0 {
		t.Fatalf("unowned hook should be a no-op: %v", result)
	}
	dir, err := threadDir(fake.thread)
	must(t, err)
	if _, err = os.Stat(filepath.Join(dir, "worker.json")); !os.IsNotExist(err) {
		t.Fatal("hook created a worker without an owner")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.resumeCalls != 0 {
		t.Fatal("unowned start attached to a backend thread")
	}
}

func TestWorkerRestartPreservesOwnerAndOwnerExitUnregisters(t *testing.T) {
	isolatedState(t)
	fake := newFakeApp(t)
	owner := exec.Command("sleep", "60")
	must(t, owner.Start())
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	ownerStart, err := processStart(owner.Process.Pid)
	must(t, err)
	_, err = Enable(fake.thread, Object{"name": "owned-peer", "app_server_socket": fake.path, "host_pid": owner.Process.Pid, "host_start": ownerStart})
	must(t, err)
	t.Cleanup(func() { _ = Stop(fake.thread) })
	must(t, Stop(fake.thread))
	info := cli(t, "start", "--thread", fake.thread)
	if int(number(info, "host_pid", 0)) != owner.Process.Pid || str(info, "host_start") != ownerStart {
		t.Fatalf("worker restart lost its owner: %v", info)
	}
	_, err = handleHook(Object{"session_id": fake.thread, "hook_event_name": "SessionStart"})
	must(t, err)
	info, err = RPC(fake.thread, "info", nil)
	must(t, err)
	if int(number(info, "host_pid", 0)) != owner.Process.Pid {
		t.Fatal("session hook replaced the client owner")
	}
	must(t, owner.Process.Kill())
	_ = owner.Wait()
	eventually(t, func() bool {
		peers, err := Discover("", 0)
		return err == nil && len(peers) == 0
	})
	dir, err := threadDir(fake.thread)
	must(t, err)
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "worker.json")); return os.IsNotExist(err) })
	if _, err = Enable(fake.thread, nil); !errors.Is(err, errOwnerGone) {
		t.Fatalf("departed owner was not identified: %v", err)
	}
	_, err = handleHook(Object{"session_id": fake.thread, "hook_event_name": "SessionStart"})
	must(t, err)
	if _, err = os.Stat(filepath.Join(dir, "worker.json")); !os.IsNotExist(err) {
		t.Fatal("session hook resurrected the orphan")
	}
	startTestPeer(t, uuid.NewString(), "owned-peer")
}

func TestHostIdentityMustMatchProcessStart(t *testing.T) {
	wrong := Config{Thread: uuid.NewString(), Delivery: "app-server", HostPID: os.Getpid(), HostStart: "a previous process"}
	if err := wrong.checkHost(); !errors.Is(err, errOwnerGone) {
		t.Fatalf("reused owner PID was not identified: %v", err)
	}
}

// Fail only the owner's observation; the process itself remains alive, and
// worker/control identity checks still use the real ps binary.
func failOwnerPS(t *testing.T, pid int, failure string) {
	t.Helper()
	realPS, err := exec.LookPath("ps")
	must(t, err)
	dir := testDir(t)
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = %s ]; then\n%s\nfi\nexec %s \"$@\"\n", shellQuote(fmt.Sprint(pid)), failure, shellQuote(realPS))
	must(t, os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWorkerSurvivesOwnerProbeFailures(t *testing.T) {
	isolatedState(t)
	fake := newFakeApp(t)
	owner := exec.Command("sleep", "60")
	must(t, owner.Start())
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	ownerStart, err := processStart(owner.Process.Pid)
	must(t, err)
	workerStart, err := processStart(os.Getpid())
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := Config{Thread: fake.thread, Name: "surviving-peer", CWD: testDir(t), Delivery: "app-server", AppSocket: fake.path, HostPID: owner.Process.Pid, HostStart: ownerStart, Lease: 60}
	w := &Worker{config: cfg, store: newTestStore(t), registry: testDir(t), pid: os.Getpid(), start: workerStart, ctx: ctx, cancel: cancel, lastTouch: time.Now(), artifacts: map[string]os.FileInfo{}}
	t.Cleanup(func() {
		if w.client != nil {
			w.client.Close()
		}
		w.cleanup()
	})
	must(t, w.maintenance())
	message := accept(t, w.store, testMessage("deliver after owner verification recovers"), "peer", "unread")
	for _, tc := range []struct{ name, failure string }{
		{"killed", "kill -KILL $$"},
		{"exit_one", "exit 1"},
		{"empty_output", "exit 0"},
		{"timeout", "exec sleep 30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failOwnerPS(t, owner.Process.Pid, tc.failure)
			// Repeated failures cannot turn an unknown observation into an exit.
			for range 2 {
				if err := w.maintenance(); !errors.Is(err, errHostUnverified) {
					t.Fatalf("expected unverifiable owner, got %v", err)
				}
				if ctx.Err() != nil {
					t.Fatal("observation failure stopped the worker")
				}
			}
			info, err := w.info()
			must(t, err)
			if str(info["activity"].(Object), "state") != "unknown" || str(info, "delivery_error") == "" || number(info, "unread", 0) != 1 {
				t.Fatalf("worker did not retain the inbox and report paused delivery: %v", info)
			}
			var ad Object
			must(t, readJSON(filepath.Join(w.registry, fmt.Sprintf("%d.json", w.pid)), false, &ad))
			if str(ad, "name") != cfg.Name || str(ad, "status") != "busy" {
				t.Fatalf("registration was lost or advertised idle: %v", ad)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.notices) != 0 {
				t.Fatal("message delivered while owner identity was unknown")
			}
		})
	}
	// The next successful observation resumes delivery without restarting.
	must(t, w.maintenance())
	info, err := w.info()
	must(t, err)
	if info["delivery_error"] != nil || str(info["activity"].(Object), "state") != "idle" {
		t.Fatalf("owner recovery did not clear degraded status: %v", info)
	}
	fake.mu.Lock()
	notices := len(fake.notices)
	fake.mu.Unlock()
	if notices != 1 {
		t.Fatalf("expected one notice after verification recovered, got %d", notices)
	}
	got, err := w.store.Get(str(message, "id"))
	must(t, err)
	if str(got, "state") != "unread" {
		t.Fatal("recovery consumed the pending message")
	}
	// Recovery must also clear its error with an existing recorded notice,
	// without generating another notice or consuming the unread message.
	t.Run("failure_with_pending_notice", func(t *testing.T) {
		failOwnerPS(t, owner.Process.Pid, "exit 1")
		if err := w.maintenance(); !errors.Is(err, errHostUnverified) {
			t.Fatalf("expected unverifiable owner, got %v", err)
		}
	})
	must(t, w.maintenance())
	info, err = w.info()
	must(t, err)
	if info["delivery_error"] != nil {
		t.Fatalf("recovery left a stale owner error: %v", info)
	}
	fake.mu.Lock()
	notices = len(fake.notices)
	fake.mu.Unlock()
	if notices != 1 {
		t.Fatalf("recovery duplicated a pending notice: %d", notices)
	}
	// A definitive owner exit still tears down the worker.
	must(t, owner.Process.Kill())
	_ = owner.Wait()
	must(t, w.maintenance())
	if ctx.Err() == nil {
		t.Fatal("worker survived a confirmed owner exit")
	}
}

func TestUnverifiedOwnerCannotStartWorker(t *testing.T) {
	isolatedState(t)
	fake := newFakeApp(t)
	start, err := processStart(os.Getpid())
	must(t, err)
	failOwnerPS(t, os.Getpid(), "exit 1")
	_, err = Enable(fake.thread, Object{"name": "unknown-owner", "app_server_socket": fake.path, "host_pid": os.Getpid(), "host_start": start})
	if !errors.Is(err, errHostUnverified) {
		t.Fatalf("start must reject an unverified owner: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.resumeCalls != 0 {
		t.Fatal("start attached to the app-server without a verified owner")
	}
}

func TestInboxAcceptanceAndReadReceipts(t *testing.T) {
	isolatedState(t)
	a, b := uuid.NewString(), uuid.NewString()
	startTestPeer(t, a, "receipt-sender")
	startTestPeer(t, b, "receipt-receiver")
	sent := cli(t, "send", "receipt-receiver", "--thread", a, "--body", "receipt test")
	id := str(sent, "message_id")
	waitForStatus := func(want string) {
		t.Helper()
		eventually(t, func() bool {
			result, err := RPC(a, "sent", Object{"message_id": id})
			if err != nil {
				return false
			}
			var rows struct{ Messages []struct{ State string } }
			if json.Unmarshal(compact(result), &rows) != nil || len(rows.Messages) != 1 {
				return false
			}
			return rows.Messages[0].State == want
		})
	}
	waitForStatus("accepted")
	inbox := cli(t, "read", "--thread", b)
	m := inbox["messages"].([]any)[0].(map[string]any)
	cli(t, "ack", str(m, "id"), "--thread", b)
	waitForStatus("read")
}

package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if _, err = Enable(fake.thread, nil); err == nil {
		t.Fatal("worker restarted under a departed owner")
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
	if err := wrong.checkHost(); err == nil {
		t.Fatal("reused owner PID was accepted")
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

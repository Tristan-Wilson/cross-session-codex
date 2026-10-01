package bridge

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A thread's saved Codex client owner stays authoritative after its messaging
// worker stops. Resuming must not start a second Codex client beside a live
// owner, and a transient ps failure must not be mistaken for an owner exit.
func TestLaunchResumeRefusesLiveOrUnverifiableOwnerWithoutWorker(t *testing.T) {
	dir := isolatedState(t)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	fake := newFakeApp(t)
	arguments := filepath.Join(dir, "client-args")
	codex := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(arguments) + "\n"
	must(t, os.WriteFile(codex, []byte(script), 0700))
	owner := exec.Command("sleep", "60")
	must(t, owner.Start())
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	ownerStart, err := processStart(owner.Process.Pid)
	must(t, err)
	_, err = Enable(fake.thread, Object{"name": "owned-peer", "app_server_socket": fake.path, "host_pid": owner.Process.Pid, "host_start": ownerStart})
	must(t, err)
	t.Cleanup(func() { _ = Stop(fake.thread) })
	exe, err := os.Executable()
	must(t, err)
	launch := func(env ...string) ([]byte, error) {
		cmd := exec.Command(exe, "launch", "--resume", fake.thread, "--codex", codex, "--app-server-socket", fake.path)
		cmd.Env = append(os.Environ(), env...)
		return cmd.CombinedOutput()
	}
	refused := func(want string, env ...string) {
		t.Helper()
		output, err := launch(env...)
		if err == nil || !strings.Contains(string(output), want) {
			t.Fatalf("launch should be refused with %q: %v %s", want, err, output)
		}
		if _, err := os.Stat(arguments); !os.IsNotExist(err) {
			t.Fatal("a second Codex client was started beside the live owner")
		}
	}
	refused("already belongs to Codex client")
	must(t, Stop(fake.thread))
	resumes := func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.resumeCalls }
	before := resumes()
	refused("already belongs to Codex client")
	if resumes() != before {
		t.Fatal("a refused launch resumed the thread in the app-server")
	}
	shim := filepath.Join(dir, "bin")
	must(t, os.MkdirAll(shim, 0700))
	must(t, os.WriteFile(filepath.Join(shim, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0700))
	refused("cannot verify", "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	must(t, owner.Process.Kill())
	_ = owner.Wait()
	output, err := launch()
	if err != nil {
		t.Fatalf("launch after the owner exited: %v %s", err, output)
	}
	got, err := os.ReadFile(arguments)
	must(t, err)
	want := "--remote\nunix://" + fake.path + "\nresume\n" + fake.thread + "\n"
	if string(got) != want {
		t.Fatalf("wrong client arguments: %s", got)
	}
	state, err := threadDir(fake.thread)
	must(t, err)
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(state, "worker.json")); return os.IsNotExist(err) })
}

// Two resumes of one thread serialize on the app-server lock. The early owner
// check cannot see an owner installed by the launch that held the lock first,
// so the saved owner is rechecked under the lock before any backend call.
func TestLaunchResumeRechecksOwnerUnderAppServerLock(t *testing.T) {
	dir := isolatedState(t)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	fake := newFakeApp(t)
	arguments := filepath.Join(dir, "client-args")
	codex := filepath.Join(dir, "fake-codex")
	must(t, os.WriteFile(codex, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+shellQuote(arguments)+"\n"), 0700))
	departed := exec.Command("sleep", "60")
	must(t, departed.Start())
	departedStart, err := processStart(departed.Process.Pid)
	must(t, err)
	must(t, departed.Process.Kill())
	_ = departed.Wait()
	state, err := threadDir(fake.thread)
	must(t, err)
	must(t, privateDir(stateRoot()))
	must(t, privateDir(state))
	cfg := Config{Thread: fake.thread, CWD: dir, Name: "raced-peer", Inbound: "accept", Permission: "bypass", Delivery: "app-server", AppSocket: fake.path, Lease: 86400, HostPID: departed.Process.Pid, HostStart: departedStart}
	must(t, atomicJSON(filepath.Join(state, "config.json"), cfg, 0600))
	// Count owner checks so the test knows when the early guard has passed.
	checks := filepath.Join(dir, "ps-calls")
	shim := filepath.Join(dir, "bin")
	must(t, os.MkdirAll(shim, 0700))
	must(t, os.WriteFile(filepath.Join(shim, "ps"), []byte("#!/bin/sh\necho check >> "+shellQuote(checks)+"\nexec /bin/ps \"$@\"\n"), 0700))
	lock, err := lockAppServer(context.Background(), fake.path)
	must(t, err)
	exe, err := os.Executable()
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "launch", "--resume", fake.thread, "--codex", codex, "--app-server-socket", fake.path)
	cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	eventually(t, func() bool { b, err := os.ReadFile(checks); return err == nil && len(b) > 0 })
	// The first launch finished while this one waited for the lock.
	cfg.HostPID = os.Getpid()
	cfg.HostStart, err = processStart(os.Getpid())
	must(t, err)
	must(t, atomicJSON(filepath.Join(state, "config.json"), cfg, 0600))
	closeQuietly(lock)
	err = cmd.Wait()
	if err == nil || !strings.Contains(output.String(), "already belongs to Codex client PID "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("launch should be refused under the lock: %v %s", err, output.String())
	}
	if _, err := os.Stat(arguments); !os.IsNotExist(err) {
		t.Fatal("a second Codex client was started beside the live owner")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.resumeCalls != 0 {
		t.Fatal("a refused launch resumed the thread in the app-server")
	}
}

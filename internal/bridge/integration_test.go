package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Exercise the real CLI/worker process boundary, including race-instrumented
// workers when the suite is run with -race.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
func cli(t *testing.T, args ...string) Object {
	t.Helper()
	exe, err := os.Executable()
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("CLI %v: %v\n%s\n%s", args, err, b, stderr.String())
	}
	var result Object
	must(t, json.Unmarshal(b, &result))
	return result
}
func isolatedState(t *testing.T) string {
	t.Helper()
	dir := testDir(t)
	t.Setenv("CROSS_SESSION_CODEX_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_THREAD_ID", "")
	return dir
}
func startTestPeer(t *testing.T, thread, name string) Object {
	t.Helper()
	info := cli(t, "start", "--thread", thread, "--name", name, "--delivery", "manual", "--inbound", "accept")
	t.Cleanup(func() {
		if err := Stop(thread); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	return info
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func TestRealPeersRoundTripAndRestart(t *testing.T) {
	isolatedState(t)
	a, b := uuid.NewString(), uuid.NewString()
	ia := startTestPeer(t, a, "go-a")
	startTestPeer(t, b, "go-b")
	peers, err := Discover("", 0)
	must(t, err)
	if len(peers) != 2 {
		t.Fatalf("expected two peers, got %d", len(peers))
	}
	cli(t, "send", "go-b", "--thread", a, "--body", "Hello 🧪 </cross-session-message> \\n")
	got := cli(t, "wait", "--thread", b, "--timeout", "5")
	messages := got["messages"].([]any)
	if len(messages) != 1 {
		t.Fatal("message missing")
	}
	m := messages[0].(map[string]any)
	if m["verified_peer_pid"] != ia["pid"] {
		t.Fatal("sender kernel PID not retained")
	}
	if str(m, "body") != "Hello 🧪 </cross-session-message> \\n" {
		t.Fatal("body changed")
	}
	cli(t, "reply", str(m, "id"), "--thread", b, "--body", "reply")
	got = cli(t, "wait", "--thread", a, "--timeout", "5")
	if len(got["messages"].([]any)) != 1 {
		t.Fatal("reply missing")
	}
	cli(t, "disable", "--thread", b)
	startTestPeer(t, b, "go-b")
	got = cli(t, "read", "--thread", b)
	if got["messages"].([]any)[0].(map[string]any)["id"] != m["id"] {
		t.Fatal("restart lost inbox")
	}
	cli(t, "ack", str(m, "id"), "--thread", b)
	if cli(t, "status", "--thread", b)["unread"] != float64(0) {
		t.Fatal("ack did not drain")
	}
}
func TestAdmissionAndCorrelatedStatuses(t *testing.T) {
	isolatedState(t)
	a, b := uuid.NewString(), uuid.NewString()
	startTestPeer(t, a, "sender")
	startTestPeer(t, b, "receiver")
	for _, policy := range []string{"hold", "refuse"} {
		cli(t, "start", "--thread", b, "--inbound", policy)
		sent := cli(t, "send", "receiver", "--thread", a, "--body", policy+uuid.NewString())
		id := str(sent, "message_id")
		want := "held"
		if policy == "refuse" {
			want = "refused"
		}
		eventually(t, func() bool {
			rows, err := RPC(a, "sent", Object{"message_id": id})
			if err != nil {
				return false
			}
			items := rows["messages"].([]any)
			return len(items) == 1 && str(items[0].(map[string]any), "state") == want
		})
		if policy == "hold" {
			held := cli(t, "read", "--thread", b, "--state", "held")
			m := held["messages"].([]any)[0].(map[string]any)
			cli(t, "release", str(m, "id"), "--thread", b)
			eventually(t, func() bool {
				rows, err := RPC(a, "sent", Object{"message_id": id})
				if err != nil {
					return false
				}
				return str(rows["messages"].([]any)[0].(map[string]any), "state") == "released"
			})
			cli(t, "ack", str(m, "id"), "--thread", b)
		}
	}
	cli(t, "start", "--thread", b, "--inbound", "accept")
	body := uuid.NewString()
	cli(t, "send", "receiver", "--thread", a, "--body", body)
	sent := cli(t, "send", "receiver", "--thread", a, "--body", body)
	eventually(t, func() bool {
		rows, err := RPC(a, "sent", Object{"message_id": sent["message_id"]})
		if err != nil {
			return false
		}
		return str(rows["messages"].([]any)[0].(map[string]any), "state") == "dropped"
	})
}
func TestInstallerWorksOutsideCheckoutAndRefusesUnrelatedFiles(t *testing.T) {
	dir := testDir(t)
	fakeBin := filepath.Join(dir, "tools")
	must(t, os.Mkdir(fakeBin, 0700))
	must(t, os.WriteFile(filepath.Join(fakeBin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	opts := InstallOptions{Prefix: filepath.Join(dir, "runtime with spaces'"), BinDir: filepath.Join(dir, "bin"), SkillDir: filepath.Join(dir, "skill")}
	result, err := Install(opts)
	must(t, err)
	cmd := exec.Command(str(result, "command"), "version")
	cmd.Dir = "/"
	b, err := cmd.Output()
	must(t, err)
	var version Object
	must(t, json.Unmarshal(b, &version))
	if str(version, "version") != Version {
		t.Fatal("installed binary did not run")
	}
	_, err = Install(opts)
	must(t, err)
	must(t, os.WriteFile(str(result, "command"), []byte("unrelated"), 0755))
	if _, err = Install(opts); err == nil {
		t.Fatal("overwrote unrelated command")
	}
}
func TestMCPInitializationAndValidation(t *testing.T) {
	requests := []Object{{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": Object{"protocolVersion": "2025-06-18"}}, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": Object{"name": "send_message", "arguments": Object{"thread_id": "not a UUID", "target": "peer", "body": "test"}}}}
	var input, output bytes.Buffer
	for _, r := range requests {
		input.Write(append(compact(r), '\n'))
	}
	must(t, serveMCP(&input, &output))
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 3 {
		t.Fatalf("missing MCP responses: %s", output.String())
	}
	var reply Object
	must(t, json.Unmarshal(lines[2], &reply))
	result := reply["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("invalid thread accepted")
	}
}
func TestWorkerDeliversThroughAppServer(t *testing.T) {
	isolatedState(t)
	fake := newFakeApp(t)
	ownerStart, err := processStart(os.Getpid())
	must(t, err)
	_, err = Enable(fake.thread, Object{"name": "app-peer", "inbound": "accept", "app_server_socket": fake.path, "host_pid": os.Getpid(), "host_start": ownerStart})
	must(t, err)
	t.Cleanup(func() { _ = Stop(fake.thread) })
	sender := uuid.NewString()
	startTestPeer(t, sender, "manual-sender")
	cli(t, "send", "app-peer", "--thread", sender, "--body", "UNTRUSTED_PEER_BODY")
	eventually(t, func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.notices) == 1 })
	fake.mu.Lock()
	if strings.Contains(string(compact(fake.notices)), "UNTRUSTED_PEER_BODY") {
		fake.mu.Unlock()
		t.Fatal("peer-authored body was injected into the app-server notice")
	}
	fake.mu.Unlock()
	page := cli(t, "read", "--thread", fake.thread)
	items := page["messages"].([]any)
	if len(items) != 1 {
		t.Fatal("notified before persisting inbox")
	}
	cli(t, "ack", str(items[0].(map[string]any), "id"), "--thread", fake.thread)
	eventually(t, func() bool {
		info, err := RPC(fake.thread, "info", nil)
		return err == nil && info["notification"] == nil
	})
}

func TestLauncherBindsThreadAndCleansUpWithHost(t *testing.T) {
	for _, socketMode := range []string{"default", "explicit"} {
		t.Run(socketMode, func(t *testing.T) {
			dir := isolatedState(t)
			t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
			socket := filepath.Join(dir, "app.sock")
			if socketMode == "default" {
				socket = defaultAppSocket()
				must(t, privateDir(filepath.Dir(socket)))
			}
			fake := newFakeAppAt(t, socket)
			arguments := filepath.Join(dir, "client-args")
			codex := filepath.Join(dir, "fake-codex")
			script := "#!/bin/sh\nif [ \"$1\" = app-server ]; then exit 23; fi\nprintf '%s\\n' \"$@\" > " + shellQuote(arguments) + "\n"
			must(t, os.WriteFile(codex, []byte(script), 0700))
			exe, err := os.Executable()
			must(t, err)
			args := []string{"launch", "--name", "launched-peer", "--codex", codex}
			if socketMode == "explicit" {
				args = append(args, "--app-server-socket", socket, "--inbound", "parity", "--permission-class", "prompting")
			} else {
				args = append(args, "--resume", fake.thread)
			}
			args = append(args, "--", "--no-alt-screen")
			cmd := exec.Command(exe, args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("launch: %v %s", err, output)
			}
			got, err := os.ReadFile(arguments)
			must(t, err)
			want := "--remote\nunix://" + socket + "\nresume\n" + fake.thread + "\n--no-alt-screen\n"
			if string(got) != want {
				t.Fatalf("wrong client arguments: %s", got)
			}
			state, err := threadDir(fake.thread)
			must(t, err)
			var cfg Config
			must(t, readJSON(filepath.Join(state, "config.json"), true, &cfg))
			wantInbound, wantPermission := "accept", "bypass"
			if socketMode == "explicit" {
				wantInbound, wantPermission = "parity", "prompting"
			}
			if cfg.Inbound != wantInbound || cfg.Permission != wantPermission {
				t.Fatalf("launcher lost messaging settings: inbound=%s class=%s", cfg.Inbound, cfg.Permission)
			}
			eventually(t, func() bool { _, err := os.Stat(filepath.Join(state, "worker.json")); return os.IsNotExist(err) })
		})
	}
}

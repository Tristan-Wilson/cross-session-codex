package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCompanionCapabilities(t *testing.T) {
	// Capability discovery must not inspect a home, create state, or require a
	// running session. These paths deliberately do not exist.
	dir := testDir(t)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "missing-home"))
	t.Setenv("CROSS_SESSION_CODEX_STATE_DIR", filepath.Join(dir, "missing-state"))
	var out, errOut bytes.Buffer
	if code := Main([]string{"capabilities"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("capabilities code=%d: %s", code, &errOut)
	}
	var got struct {
		Name     string   `json:"name"`
		Version  string   `json:"version"`
		CLIAPI   int      `json:"cli_api"`
		Features []string `json:"features"`
	}
	must(t, json.Unmarshal(out.Bytes(), &got))
	if got.Name != "cross-session-codex" || got.Version != Version || got.CLIAPI != 1 || len(got.Features) != 1 || got.Features[0] != "launch-handshake-v1" {
		t.Fatalf("unexpected capabilities: %+v", got)
	}
	for _, path := range []string{"missing-home", "missing-state"} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("capabilities touched %s: %v", path, err)
		}
	}
	if code := Main([]string{"capabilities", "unexpected"}, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("capabilities accepted arguments")
	}
}

func TestLaunchRejectsIncompleteHandoffBeforePreparation(t *testing.T) {
	for _, args := range [][]string{
		{"--ready-fd", "3"}, {"--continue-fd", "4"},
		{"--ready-fd", "-1", "--continue-fd", "-1"},
		{"--ready-fd", "1", "--continue-fd", "4"},
	} {
		var out, errOut bytes.Buffer
		if code := Main(append([]string{"launch"}, args...), strings.NewReader(""), &out, &errOut); code == 0 {
			t.Fatalf("accepted invalid descriptors: %v", args)
		}
		if !strings.Contains(errOut.String(), "pipe descriptors at least 3") {
			t.Fatalf("unexpected failure for %v: %s", args, &errOut)
		}
	}
}

func TestLauncherHandoffOwnsClientThroughExecAndCancellation(t *testing.T) {
	for _, action := range []string{"continue", "cancel", "eof", "wrong-nonce", "signal", "exec-failure"} {
		t.Run(action, func(t *testing.T) {
			dir := isolatedState(t)
			t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
			fake := newFakeApp(t)
			argsPath := filepath.Join(dir, "ui-args")
			pidPath := filepath.Join(dir, "ui-pid")
			codex := filepath.Join(dir, "fake-codex")
			script := "#!/bin/sh\n" +
				"if [ -e /dev/fd/3 ] || [ -e /dev/fd/4 ]; then exit 31; fi\n" +
				"printf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "\n" +
				"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n"
			must(t, os.WriteFile(codex, []byte(script), 0700))
			readyR, readyW, err := os.Pipe()
			must(t, err)
			continueR, continueW, err := os.Pipe()
			must(t, err)
			t.Cleanup(func() {
				closeQuietly(readyR)
				closeQuietly(readyW)
				closeQuietly(continueR)
				closeQuietly(continueW)
			})
			exe, err := os.Executable()
			must(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			args := []string{"launch", "--name", "handoff-peer", "--codex", codex, "--app-server-socket", fake.path, "--ready-fd", "3", "--continue-fd", "4"}
			if action == "continue" {
				args = append(args, "--resume", fake.thread)
			}
			cmd := exec.CommandContext(ctx, exe, args...)
			cmd.ExtraFiles = []*os.File{readyW, continueR}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			must(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			closeQuietly(readyW)
			closeQuietly(continueR)
			line, err := bufio.NewReader(readyR).ReadBytes('\n')
			must(t, err)
			var ready struct {
				V               int    `json:"v"`
				Event           string `json:"event"`
				Nonce           string `json:"nonce"`
				ThreadID        string `json:"thread_id"`
				CodexHome       string `json:"codex_home"`
				AppServerSocket string `json:"app_server_socket"`
				OwnerPID        int    `json:"owner_pid"`
				OwnerStart      string `json:"owner_start"`
				Name            string `json:"name"`
				Version         string `json:"version"`
			}
			must(t, json.Unmarshal(line, &ready))
			if ready.V != 1 || ready.Event != "ready" || ready.Nonce == "" || ready.ThreadID != fake.thread || ready.CodexHome != os.Getenv("CODEX_HOME") || ready.AppServerSocket != fake.path || ready.OwnerPID != cmd.Process.Pid || ready.OwnerStart == "" || ready.Name != "handoff-peer" || ready.Version != Version {
				t.Fatalf("invalid ready frame: %+v", ready)
			}
			if _, err = os.Stat(argsPath); !os.IsNotExist(err) {
				t.Fatalf("UI ran before continuation: %v", err)
			}
			info, err := RPC(fake.thread, "info", nil)
			must(t, err)
			if str(info, "name") != ready.Name {
				t.Fatalf("peer not ready: %+v", info)
			}
			// The guarded launch/shutdown lock remains held, but an independent
			// client can validate/resume the exact thread without deadlocking.
			lockCtx, cancelLock := context.WithTimeout(ctx, 75*time.Millisecond)
			lock, lockErr := lockAppServer(lockCtx, fake.path)
			cancelLock()
			if lock != nil {
				closeQuietly(lock)
			}
			if !errors.Is(lockErr, context.DeadlineExceeded) {
				t.Fatalf("handoff did not hold lifecycle lock: %v", lockErr)
			}
			client := fake.client(t)
			var result Object
			must(t, client.call(ctx, "thread/resume", Object{"threadId": ready.ThreadID, "excludeTurns": true}, &result))
			switch action {
			case "eof":
			case "signal":
				must(t, cmd.Process.Signal(syscall.SIGTERM))
			default:
				nonce, next := ready.Nonce, action
				if action == "wrong-nonce" {
					nonce, next = "not-the-nonce", "continue"
				}
				if action == "exec-failure" {
					must(t, os.Remove(codex))
					next = "continue"
				}
				must(t, json.NewEncoder(continueW).Encode(Object{"v": 1, "nonce": nonce, "action": next}))
			}
			if action != "signal" {
				closeQuietly(continueW)
			}
			err = cmd.Wait()
			closeQuietly(continueW)
			if action == "continue" {
				if err != nil {
					t.Fatalf("continue failed: %v\n%s", err, &output)
				}
				pid, readErr := os.ReadFile(pidPath)
				must(t, readErr)
				if strings.TrimSpace(string(pid)) != strconv.Itoa(ready.OwnerPID) {
					t.Fatalf("UI lost launcher ownership: %s != %d", pid, ready.OwnerPID)
				}
				got, readErr := os.ReadFile(argsPath)
				must(t, readErr)
				if string(got) != "--remote\nunix://"+fake.path+"\nresume\n"+fake.thread+"\n" {
					t.Fatalf("wrong UI destination: %s", got)
				}
			} else {
				if err == nil {
					t.Fatalf("%s unexpectedly succeeded: %s", action, &output)
				}
				if _, err = os.Stat(argsPath); !os.IsNotExist(err) {
					t.Fatalf("canceled handoff launched UI: %v", err)
				}
			}
			state, err := threadDir(fake.thread)
			must(t, err)
			eventually(t, func() bool {
				_, statErr := os.Stat(filepath.Join(state, "worker.json"))
				return os.IsNotExist(statErr)
			})
			_, err = os.Stat(filepath.Join(state, "inbox.sqlite3"))
			must(t, err)
			_, err = os.Stat(filepath.Join(state, "config.json"))
			must(t, err)
			peers, err := Discover("", 0)
			must(t, err)
			if len(peers) != 0 {
				t.Fatalf("peer name leaked after owner exit: %+v", peers)
			}
			must(t, client.call(ctx, "thread/read", Object{"threadId": ready.ThreadID}, &result))
		})
	}
}

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This test uses the installed Codex executable with an isolated CODEX_HOME and
// a local fake Responses endpoint. It never contacts a model or uses credentials.
func TestLiveCodexAppServer(t *testing.T) {
	if os.Getenv("CSC_TEST_CODEX") != "1" {
		t.Skip("set CSC_TEST_CODEX=1 to test the installed Codex app-server")
	}
	codex, err := exec.LookPath("codex")
	must(t, err)
	dir := testDir(t)
	t.Setenv("CROSS_SESSION_CODEX_STATE_DIR", filepath.Join(dir, "state"))
	codexHome := filepath.Join(dir, "codex")
	must(t, privateDir(codexHome))
	var requests atomic.Int32
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(releaseFirst)
		}
	}
	defer release()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		emit := func(kind string, data Object) {
			data["type"] = kind
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, compact(data))
			flusher.Flush()
		}
		id := fmt.Sprintf("resp_smoke_%d", n)
		emit("response.created", Object{"response": Object{"id": id, "status": "in_progress"}})
		if n == 1 {
			close(firstRequest)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		}
		item := Object{"id": fmt.Sprintf("msg_smoke_%d", n), "type": "message", "role": "assistant", "status": "completed", "content": []Object{{"type": "output_text", "text": "Smoke receipt.", "annotations": []any{}}}}
		emit("response.output_item.added", Object{"output_index": 0, "item": Object{"id": item["id"], "type": "message", "role": "assistant", "content": []any{}}})
		emit("response.output_text.delta", Object{"output_index": 0, "content_index": 0, "delta": "Smoke receipt."})
		emit("response.output_item.done", Object{"output_index": 0, "item": item})
		emit("response.completed", Object{"response": Object{"id": id, "status": "completed", "output": []Object{item}, "usage": Object{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer provider.Close()
	config := fmt.Sprintf("model = \"smoke\"\nmodel_provider = \"smoke\"\n[model_providers.smoke]\nname = \"Local test provider\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\nrequest_max_retries = 0\nstream_max_retries = 0\n", provider.URL+"/v1")
	must(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	socket := defaultAppSocket()
	must(t, privateDir(filepath.Dir(socket)))
	// Leave a stale socket, as after an interrupted server, to exercise recovery.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	must(t, err)
	stale.SetUnlinkOnClose(false)
	must(t, stale.Close())
	must(t, os.Chmod(socket, 0600))
	pidPath := filepath.Join(dir, "server.pid")
	wrapper := filepath.Join(dir, "codex-wrapper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "\nexec " + shellQuote(codex) + " \"$@\"\n"
	must(t, os.WriteFile(wrapper, []byte(script), 0700))
	defer func() {
		stopAppServerFixture(t, pidPath)
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(filepath.Dir(socket), "cross-session-codex.log"))
			t.Logf("Codex smoke log:\n%s", b)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	client, err := ensureAppServer(ctx, wrapper, socket, dir)
	must(t, err)
	defer client.Close()
	// Upgrade compatibility: the previously installed launcher did not write a
	// PID record, so shutdown must verify its exact direct-launch command instead.
	must(t, os.Remove(appServerRecordPath(socket)))
	var response struct {
		Thread appThread `json:"thread"`
	}
	must(t, client.call(ctx, "thread/start", Object{"cwd": dir, "ephemeral": false}, &response))
	thread := response.Thread.ID
	must(t, client.Bootstrap(ctx, thread))
	_, err = client.Attach(ctx, thread)
	must(t, err)
	client.Watch(thread, "cross_session_smoke_idle")
	must(t, client.Notice(ctx, thread, "cross_session_smoke_idle", "Local smoke test: record this fixed tool output."))
	select {
	case <-firstRequest:
	case <-ctx.Done():
		t.Fatal("idle notice did not start a model request")
	}
	active, err := client.ReadThread(ctx, thread)
	must(t, err)
	if active.Status.Type != "active" {
		t.Fatalf("expected active turn, got %s", active.Status.Type)
	}
	shutdown, err := Shutdown(ctx, socket, true)
	must(t, err)
	if str(shutdown, "status") != "busy" || len(shutdown["active_threads"].([]string)) == 0 {
		t.Fatalf("shutdown missed an active Codex turn: %v", shutdown)
	}
	must(t, client.Notice(ctx, thread, "cross_session_smoke_active", "Second fixed tool output, submitted during an active turn."))
	release()
	eventually(t, func() bool {
		var page struct {
			Data []struct {
				Item Object `json:"item"`
			} `json:"data"`
		}
		if client.call(ctx, "thread/items/list", Object{"threadId": thread, "limit": 100, "sortDirection": "desc"}, &page) != nil {
			return false
		}
		names := map[string]bool{}
		for _, entry := range page.Data {
			if str(entry.Item, "type") == "functionCallOutput" {
				names[str(entry.Item, "name")] = true
			}
		}
		return names["cross_session_smoke_idle"] && names["cross_session_smoke_active"]
	})
	client.Close()
	client, err = dialApp(ctx, socket)
	must(t, err)
	defer client.Close()
	_, err = client.Attach(ctx, thread)
	must(t, err)
	client.Watch(thread, "cross_session_smoke_active")
	found, err := client.HasNotice(ctx, thread, "cross_session_smoke_active")
	must(t, err)
	if !found {
		t.Fatal("receipt not recoverable after reconnect")
	}
	eventually(t, func() bool {
		thread, err := client.ReadThread(ctx, thread)
		return err == nil && thread.Status.Type == "idle"
	})
	client.Close()
	eventually(t, func() bool {
		info, err := Shutdown(ctx, socket, true)
		return err == nil && str(info, "status") == "ready"
	})
	shutdown, err = Shutdown(ctx, socket, false)
	must(t, err)
	if str(shutdown, "status") != "stopped" {
		t.Fatalf("Codex did not stop gracefully: %v", shutdown)
	}
	client, err = ensureAppServer(ctx, wrapper, socket, dir)
	must(t, err)
	client.Close()
	shutdown, err = Shutdown(ctx, socket, false)
	must(t, err)
	if str(shutdown, "status") != "stopped" {
		t.Fatalf("Codex did not stop after restart: %v", shutdown)
	}
	encoded, _ := json.Marshal(Object{"idle_wake": true, "active_queue": true, "reconnect_receipt": true, "model_requests": requests.Load()})
	t.Log(string(encoded))
}

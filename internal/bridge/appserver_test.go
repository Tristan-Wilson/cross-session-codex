package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

type fakeApp struct {
	mu                            sync.Mutex
	path, thread, status          string
	loaded                        bool
	notices                       []Object
	names                         []string
	resumeCalls                   int
	dropReply, reject, omitRecord bool
}

func newFakeApp(t *testing.T) *fakeApp {
	t.Helper()
	return newFakeAppAt(t, filepath.Join(testDir(t), "app.sock"))
}

func newFakeAppAt(t *testing.T, path string) *fakeApp {
	t.Helper()
	f := &fakeApp{path: path, thread: uuid.NewString(), status: "idle", loaded: true}
	l, err := net.Listen("unix", f.path)
	must(t, err)
	must(t, os.Chmod(f.path, 0600))
	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		c.SetReadLimit(64 << 20)
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params Object          `json:"params"`
			}
			if json.Unmarshal(b, &req) != nil {
				return
			}
			if len(req.ID) == 0 {
				continue
			}
			f.mu.Lock()
			var result any
			var rpcErr any
			drop := false
			var event Object
			switch req.Method {
			case "initialize":
				result = Object{"userAgent": "fake"}
			case "thread/inject_items":
				result = Object{}
			case "thread/loaded/list":
				ids := []string{}
				if f.loaded {
					ids = append(ids, f.thread)
				}
				result = Object{"data": ids}
			case "thread/read", "thread/resume", "thread/start":
				if req.Method == "thread/resume" {
					f.resumeCalls++
				}
				status := f.status
				if !f.loaded {
					status = "notLoaded"
				}
				result = Object{"thread": Object{"id": f.thread, "cwd": "/tmp", "ephemeral": false, "canAcceptDirectInput": true, "status": Object{"type": status}}}
			case "turn/start":
				f.notices = append(f.notices, req.Params)
				tool, _ := req.Params["toolOutput"].(map[string]any)
				if f.reject {
					rpcErr = Object{"code": -32602, "message": "rejected by test server"}
				} else if !f.omitRecord {
					f.names = append(f.names, str(tool, "name"))
					event = Object{"method": "item/completed", "params": Object{"threadId": f.thread, "turnId": "turn", "item": Object{"type": "functionCallOutput", "name": tool["name"]}}}
				}
				result = Object{"turn": Object{"id": "turn", "status": "inProgress"}}
				drop = f.dropReply
			case "thread/items/list":
				items := []Object{}
				for _, name := range f.names {
					items = append(items, Object{"turnId": "turn", "item": Object{"type": "functionCallOutput", "name": name}})
				}
				result = Object{"data": items}
			default:
				rpcErr = Object{"code": -32601, "message": "unknown method"}
			}
			f.mu.Unlock()
			if drop {
				return
			}
			if event != nil {
				if c.Write(ctx, websocket.MessageText, compact(event)) != nil {
					return
				}
			}
			response := Object{"id": req.ID, "result": result}
			if rpcErr != nil {
				response = Object{"id": req.ID, "error": rpcErr}
			}
			if c.Write(ctx, websocket.MessageText, compact(response)) != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { cancel(); _ = server.Close(); _ = l.Close() })
	return f
}
func (f *fakeApp) client(t *testing.T) *appClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialApp(ctx, f.path)
	must(t, err)
	t.Cleanup(c.Close)
	return c
}
func TestAppServerExactThreadAttachment(t *testing.T) {
	f := newFakeApp(t)
	c := f.client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Attach(ctx, uuid.NewString()); err == nil {
		t.Fatal("attached to absent thread")
	}
	f.mu.Lock()
	calls := f.resumeCalls
	f.mu.Unlock()
	if calls != 0 {
		t.Fatal("resumed a saved thread during preflight")
	}
	thread, err := c.Attach(ctx, f.thread)
	must(t, err)
	if thread.ID != f.thread {
		t.Fatal("wrong thread")
	}
	f.mu.Lock()
	f.loaded = false
	f.mu.Unlock()
	if _, err = c.Attach(ctx, f.thread); err == nil {
		t.Fatal("attached to unloaded thread")
	}
}
func TestAppServerNoticeIdleAndActive(t *testing.T) {
	for _, status := range []string{"idle", "active"} {
		t.Run(status, func(t *testing.T) {
			f := newFakeApp(t)
			f.mu.Lock()
			f.status = status
			f.mu.Unlock()
			c := f.client(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.Attach(ctx, f.thread)
			must(t, err)
			name := "cross_session_inbox_test"
			c.Watch(f.thread, name)
			must(t, c.Notice(ctx, f.thread, name, "fixed inbox notice"))
			found, err := c.HasNotice(ctx, f.thread, name)
			must(t, err)
			if !found {
				t.Fatal("notice receipt not correlated")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.notices) != 1 {
				t.Fatal("duplicate notice")
			}
			request := f.notices[0]
			if len(request["input"].([]any)) != 0 || str(request, "threadId") != f.thread {
				t.Fatal("notice became user input or changed thread")
			}
			if len(request) != 3 {
				t.Fatal("notice changed model, permissions or other session configuration")
			}
		})
	}
}
func notificationWorker(t *testing.T, f *fakeApp) *Worker {
	t.Helper()
	w := &Worker{store: newTestStore(t), client: f.client(t)}
	w.config = Config{Thread: f.thread, Executable: "/installed/cross-session-codex", Delivery: "app-server"}
	accept(t, w.store, testMessage("peer body is not the notice"), "p", "unread")
	return w
}
func TestAmbiguousDeliveryReconcilesWithoutReplay(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprint(recorded), func(t *testing.T) {
			f := newFakeApp(t)
			f.mu.Lock()
			f.dropReply = true
			f.omitRecord = !recorded
			f.mu.Unlock()
			w := notificationWorker(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			must(t, w.notify(ctx, w.config))
			var wake Object
			must(t, w.store.Meta("wake", &wake))
			if str(wake, "state") != "uncertain" {
				t.Fatalf("lost write certainty: %#v", wake)
			}
			w.client.Close()
			w.client = f.client(t)
			w.lastReconcile = time.Time{}
			must(t, w.notify(ctx, w.config))
			wake = nil
			must(t, w.store.Meta("wake", &wake))
			if recorded && str(wake, "state") != "recorded" {
				t.Fatal("history did not recover accepted notice")
			}
			if !recorded && wake["needs_manual_action"] != true {
				t.Fatal("ambiguous write was marked safe")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.notices) != 1 {
				t.Fatal("ambiguous notice replayed")
			}
		})
	}
}
func TestRejectedNoticeBackoffAndProgress(t *testing.T) {
	f := newFakeApp(t)
	f.mu.Lock()
	f.reject = true
	f.mu.Unlock()
	w := notificationWorker(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	must(t, w.notify(ctx, w.config))
	must(t, w.notify(ctx, w.config))
	f.mu.Lock()
	if len(f.notices) != 1 {
		t.Fatal("rejected request retried without backoff")
	}
	f.reject = false
	f.mu.Unlock()
	must(t, w.store.SetMeta("retry_at", 0))
	must(t, w.notify(ctx, w.config))
	var wake Object
	must(t, w.store.Meta("wake", &wake))
	if wake["submitted"] != true {
		t.Fatal("accepted request not recorded")
	}
	second := accept(t, w.store, testMessage("second"), "p", "unread")
	items, err := w.store.Read("unread", 20)
	must(t, err)
	_, err = w.store.Transition([]string{str(items[0], "id")}, "unread", "read")
	must(t, err)
	must(t, w.notify(ctx, w.config))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.notices) != 3 {
		t.Fatalf("remaining inbox did not wake after partial ack: %d (%s)", len(f.notices), second["id"])
	}
}

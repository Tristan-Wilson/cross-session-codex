package bridge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const coexistenceThread = "01912345-6789-7abc-8def-0123456789ab"

func TestCrossSessionReceiptIgnoresBotbusNotifications(t *testing.T) {
	c := coexistenceApp(t)
	const name = "cross_session_inbox_coexistence"
	c.Watch(coexistenceThread, name)
	for _, test := range []struct {
		name, method, thread string
		item                 Object
	}{
		{
			name: "Botbus user input", method: "item/completed", thread: coexistenceThread,
			item: Object{"type": "userMessage", "clientId": "botbus-coexistence-1"},
		},
		{
			name: "user input with matching name", method: "item/completed", thread: coexistenceThread,
			item: Object{"type": "userMessage", "clientId": "botbus-coexistence-1", "name": name},
		},
		{
			name: "notice for another thread", method: "item/completed", thread: "01912345-6789-7abc-8def-0123456789ac",
			item: Object{"type": "functionCallOutput", "name": name},
		},
		{
			name: "another notice for this thread", method: "item/completed", thread: coexistenceThread,
			item: Object{"type": "functionCallOutput", "name": "cross_session_inbox_other"},
		},
		{
			name: "unfinished matching notice", method: "item/started", thread: coexistenceThread,
			item: Object{"type": "functionCallOutput", "name": name},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coexistenceCall(t, c, "test/notification", Object{
				"method": test.method, "params": Object{"threadId": test.thread, "item": test.item},
			})
			if c.Recorded() {
				t.Fatal("unrelated notification confirmed the local inbox notice")
			}
		})
	}
	coexistenceCall(t, c, "test/notification", Object{
		"method": "item/completed", "params": Object{
			"threadId": coexistenceThread, "item": Object{"type": "functionCallOutput", "name": name},
		},
	})
	if !c.Recorded() {
		t.Fatal("matching completed tool output did not confirm the local inbox notice")
	}
}

func TestCrossSessionReceiptIgnoresBotbusHistory(t *testing.T) {
	c := coexistenceApp(t)
	const name = "cross_session_inbox_coexistence"
	c.Watch(coexistenceThread, name)
	items := []Object{
		{"item": Object{"type": "userMessage", "clientId": "botbus-coexistence-1", "name": name}},
		{"item": Object{"type": "agentMessage", "name": name}},
		{"item": Object{"type": "functionCallOutput", "name": "cross_session_inbox_other", "output": name}},
	}
	coexistenceCall(t, c, "test/history", Object{"items": items})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found, err := c.HasNotice(ctx, coexistenceThread, name)
	must(t, err)
	if found {
		t.Fatal("unrelated history item confirmed the local inbox notice")
	}
	items = append(items, Object{"item": Object{"type": "functionCallOutput", "name": name}})
	coexistenceCall(t, c, "test/history", Object{"items": items})
	found, err = c.HasNotice(ctx, coexistenceThread, name)
	must(t, err)
	if !found {
		t.Fatal("matching tool-output history item did not confirm the local inbox notice")
	}
	if c.Recorded() {
		t.Fatal("fixture unexpectedly emitted a notice notification; history fallback was not isolated")
	}
}

// This dedicated fixture leaves the general app-server fake unchanged. Each
// test-only notification precedes its RPC response on the same WebSocket, so
// the client has processed every decoy before an assertion runs, without sleeps.
func coexistenceApp(t *testing.T) *appClient {
	t.Helper()
	path := filepath.Join(testDir(t), "coexist.sock")
	listener, err := net.Listen("unix", path)
	must(t, err)
	must(t, os.Chmod(path, 0600))
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		var items any = []Object{}
		for {
			_, body, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params Object          `json:"params"`
			}
			if json.Unmarshal(body, &request) != nil || len(request.ID) == 0 {
				continue
			}
			response := Object{"id": request.ID, "result": Object{}}
			switch request.Method {
			case "initialize":
			case "test/notification":
				if conn.Write(context.Background(), websocket.MessageText, compact(request.Params)) != nil {
					return
				}
			case "test/history":
				items = request.Params["items"]
			case "thread/items/list":
				if str(request.Params, "threadId") == coexistenceThread {
					response["result"] = Object{"data": items}
				} else {
					response = Object{"id": request.ID, "error": Object{"code": -32602, "message": "wrong thread"}}
				}
			default:
				response = Object{"id": request.ID, "error": Object{"code": -32601, "message": "unexpected method"}}
			}
			if conn.Write(context.Background(), websocket.MessageText, compact(response)) != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialApp(ctx, path)
	must(t, err)
	t.Cleanup(c.Close)
	return c
}

func coexistenceCall(t *testing.T, c *appClient, method string, params Object) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	must(t, c.call(ctx, method, params, nil))
}

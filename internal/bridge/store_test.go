package bridge

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(filepath.Join(testDir(t), "inbox.sqlite3"))
	must(t, err)
	t.Cleanup(func() { closeQuietly(s) })
	return s
}
func testMessage(body string) Object {
	return Object{"wire_id": uuid.NewString(), "body": body, "raw_envelope": body, "from": "uds:/tmp/cc-socks/123.sock", "from_mode": "prompting"}
}
func accept(t *testing.T, s *Store, m Object, identity, state string) Object {
	t.Helper()
	got, reason, _, err := s.Accept(m, identity, state)
	must(t, err)
	if reason != "" {
		t.Fatal(reason)
	}
	return got
}
func TestStoreDedupRateAndQueueBounds(t *testing.T) {
	s := newTestStore(t)
	m := testMessage("same")
	accept(t, s, m, "peer", "unread")
	for _, duplicate := range []Object{m, testMessage("same")} {
		_, reason, _, err := s.Accept(duplicate, "peer", "unread")
		must(t, err)
		if reason != "duplicate" {
			t.Fatal(reason)
		}
	}
	for i := 0; i < 19; i++ {
		accept(t, s, testMessage(uuid.NewString()), "peer", "unread")
	}
	_, reason, _, err := s.Accept(testMessage("limited"), "peer", "unread")
	must(t, err)
	if reason != "rate_limit" {
		t.Fatal(reason)
	}
	for i := 0; i < 30; i++ {
		accept(t, s, testMessage(uuid.NewString()), uuid.NewString(), "unread")
	}
	_, reason, _, err = s.Accept(testMessage("full"), "other", "unread")
	must(t, err)
	if reason != "full_queue" {
		t.Fatal(reason)
	}
	first := accept(t, s, testMessage("held first"), "held-peer", "held")
	for i := 0; i < 99; i++ {
		accept(t, s, testMessage(uuid.NewString()), uuid.NewString(), "held")
	}
	_, reason, evicted, err := s.Accept(testMessage("last"), "new-peer", "held")
	must(t, err)
	if reason != "" || evicted["id"] != first["id"] {
		t.Fatal("held queue did not evict oldest")
	}
}
func TestAcknowledgementAndDeliveryRaces(t *testing.T) {
	for _, state := range []string{"submitted", "uncertain", "recorded"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			first := accept(t, s, testMessage("a"), "p", "unread")
			accept(t, s, testMessage("b"), "p", "unread")
			wake, err := s.BeginWake(uuid.NewString())
			must(t, err)
			_, err = s.Transition([]string{str(first, "id")}, "unread", "read")
			must(t, err)
			must(t, s.FinishWake(str(wake, "id"), state, "result"))
			var stored Object
			must(t, s.Meta("wake", &stored))
			if state == "uncertain" && stored == nil {
				t.Fatal("uncertain wake was replayable after partial ack")
			}
			if state != "uncertain" && stored != nil {
				t.Fatal("ack race relatched a successful wake")
			}
			remaining, err := s.Read("unread", 20)
			must(t, err)
			_, err = s.Transition([]string{str(remaining[0], "id")}, "unread", "read")
			must(t, err)
			stored = nil
			must(t, s.Meta("wake", &stored))
			if stored != nil {
				t.Fatal("empty inbox still has wake")
			}
		})
	}
}
func TestConcurrentAcceptAndAcknowledgement(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	errCh := make(chan error, 80)
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := s.Accept(testMessage(uuid.NewString()), uuid.NewString(), "unread")
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		must(t, err)
	}
	n, err := s.Count("unread")
	must(t, err)
	if n != 50 {
		t.Fatalf("queue limit raced: %d", n)
	}
	items, err := s.Read("unread", 50)
	must(t, err)
	for _, m := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Transition([]string{str(m, "id")}, "unread", "read")
			if e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	var revision int
	must(t, s.Meta("ack_revision", &revision))
	if revision != 50 {
		t.Fatalf("lost ack revision: %d", revision)
	}
}
func TestReadPagesBoundBytesAndPreserveCursor(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 10; i++ {
		body := uuid.NewString() + strings.Repeat("\"\\\n", 100000)
		accept(t, s, testMessage(body), "p", "unread")
	}
	page, err := s.Page("unread", "", 50, Object{})
	must(t, err)
	items := page["messages"].([]Object)
	if len(items) == 0 || len(items) >= 10 || len(compact(Object{"result": page}))+1 > MaxReadResponse {
		t.Fatal("page byte cap failed")
	}
	seen := len(items)
	cursor := str(page, "next_after")
	for cursor != "" {
		page, err = s.Page("unread", cursor, 50, Object{})
		must(t, err)
		seen += len(page["messages"].([]Object))
		cursor = str(page, "next_after")
	}
	if seen != 10 {
		t.Fatalf("paging lost messages: %d", seen)
	}
	if _, err = s.Page("unread", "missing", 20, Object{}); err == nil {
		t.Fatal("unknown cursor accepted")
	}
	n, err := s.Count("unread")
	must(t, err)
	if n != 10 {
		t.Fatal("read consumed messages")
	}
}
func TestPersistedSchemaAndWakeRecovery(t *testing.T) {
	path := filepath.Join(testDir(t), "inbox.sqlite3")
	s, err := openStore(path)
	must(t, err)
	// Seed the on-disk JSON/REAL representation independently of Store.Accept.
	m := testMessage("legacy history")
	m["id"] = uuid.NewString()
	m["received_at"] = float64(time.Now().Unix())
	raw, err := json.Marshal(m)
	must(t, err)
	_, err = s.db.Exec("INSERT INTO inbox VALUES (?,?,?,?,?,?)", m["id"], m["wire_id"], "123:legacy start", m["received_at"], "unread", string(raw))
	must(t, err)
	wake, err := s.BeginWake(uuid.NewString())
	must(t, err)
	must(t, s.Close())
	s, err = openStore(path)
	must(t, err)
	defer closeQuietly(s)
	items, err := s.Read("unread", 20)
	must(t, err)
	if len(items) != 1 || items[0]["body"] != "legacy history" {
		t.Fatal("legacy history changed")
	}
	var pending Object
	must(t, s.Meta("wake", &pending))
	if pending["id"] != wake["id"] {
		t.Fatal("pending attempt lost across restart")
	}
	if next, err := s.BeginWake(uuid.NewString()); err != nil || next != nil {
		t.Fatal("ambiguous attempt was replayed")
	}
}
func TestOutboundStatusRequiresMatchingProcess(t *testing.T) {
	s := newTestStore(t)
	frame := Object{"msg_id": uuid.NewString()}
	target := Peer{PID: 321, Start: "expected"}
	must(t, s.Outgoing(frame, target))
	status := Object{"status": "held", "orig_msg_id": frame["msg_id"]}
	must(t, s.Correlate(status, 321, "reused"))
	rows, err := s.SentStatus(str(frame, "msg_id"))
	must(t, err)
	if rows[0]["state"] != "sending" {
		t.Fatal("unverified status accepted")
	}
	must(t, s.Correlate(status, 321, "expected"))
	must(t, s.Sent(str(frame, "msg_id"), "sent_unconfirmed", nil))
	rows, err = s.SentStatus(str(frame, "msg_id"))
	must(t, err)
	if rows[0]["state"] != "held" {
		t.Fatal("send completion overwrote recipient status")
	}
}

func TestReadReceiptCannotBeForgedOrDowngraded(t *testing.T) {
	s := newTestStore(t)
	frame := Object{"msg_id": uuid.NewString()}
	must(t, s.Outgoing(frame, Peer{PID: 123, Start: "expected"}))
	status := Object{"status": "read", "orig_msg_id": frame["msg_id"]}
	must(t, s.Correlate(status, 123, "other process"))
	rows, err := s.SentStatus(str(frame, "msg_id"))
	must(t, err)
	if rows[0]["state"] != "sending" {
		t.Fatal("receipt from a reused PID was accepted")
	}
	must(t, s.Correlate(status, 123, "expected"))
	for _, stale := range []string{"accepted", "held", "released", "expired"} {
		status["status"] = stale
		must(t, s.Correlate(status, 123, "expected"))
	}
	must(t, s.Sent(str(frame, "msg_id"), "sent_unconfirmed", nil))
	must(t, s.Sent(str(frame, "msg_id"), "failed", "late write error"))
	rows, err = s.SentStatus(str(frame, "msg_id"))
	must(t, err)
	if rows[0]["state"] != "read" {
		t.Fatalf("late status downgraded acknowledged receipt: %v", rows)
	}
}
func TestHeldExpiryAndAtomicReleaseCapacity(t *testing.T) {
	s := newTestStore(t)
	m := accept(t, s, testMessage("old"), "p", "held")
	_, err := s.db.Exec("UPDATE inbox SET received=? WHERE id=?", float64(time.Now().Unix()-301), m["id"])
	must(t, err)
	// Expiry uses the persistent received_at data too.
	m["received_at"] = float64(time.Now().Unix() - 301)
	_, err = s.db.Exec("UPDATE inbox SET data=? WHERE id=?", string(compact(m)), m["id"])
	must(t, err)
	expired, err := s.Expire()
	must(t, err)
	if len(expired) != 1 {
		t.Fatal("held message did not expire")
	}
	for i := 0; i < 50; i++ {
		accept(t, s, testMessage(uuid.NewString()), uuid.NewString(), "unread")
	}
	held := accept(t, s, testMessage("held"), "p", "held")
	if _, err = s.Transition([]string{str(held, "id")}, "held", "unread"); err == nil {
		t.Fatal("release overflowed accepted queue")
	}
	got, err := s.Get(str(held, "id"))
	must(t, err)
	if got["state"] != "held" {
		t.Fatal("failed release changed state")
	}
}

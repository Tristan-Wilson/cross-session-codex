package bridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type statusRequest struct {
	message        Object
	status, reason string
	ids            []string
}
type Worker struct {
	dir, registry, path, control, address, start string
	pid                                          int
	born                                         int64
	store                                        *Store
	ctx                                          context.Context
	cancel                                       context.CancelFunc
	mu                                           sync.RWMutex
	config                                       Config
	activity                                     Object
	lastTouch                                    time.Time
	deliveryMu                                   sync.Mutex
	client                                       *appClient
	attached                                     bool
	lastReconcile                                time.Time
	artifactMu                                   sync.Mutex
	artifacts                                    map[string]os.FileInfo
	statusQueue                                  chan statusRequest
	dropMu                                       sync.Mutex
	drops                                        map[string][]Object
	changed                                      chan struct{}
	wg                                           sync.WaitGroup
}

func logEvent(event string, err error) {
	record := Object{"time": time.Now().UTC().Format(time.RFC3339), "event": event}
	if err != nil {
		record["error"] = err.Error()
	}
	_, _ = fmt.Fprintln(os.Stderr, string(compact(record)))
}
func RunWorker(dir string) error {
	unix.Umask(0077)
	if err := privateDir(dir); err != nil {
		return err
	}
	var cfg Config
	if err := readJSON(filepath.Join(dir, "config.json"), true, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.checkHost(); err != nil {
		return err
	}
	if filepath.Base(dir) != cfg.Thread {
		return errors.New("worker directory does not match thread")
	}
	registry := sessionsDir()
	if err := privateDir(registry); err != nil {
		return err
	}
	sockets, err := chooseSocketDir()
	if err != nil {
		return err
	}
	pid := os.Getpid()
	start, err := processStart(pid)
	if err != nil {
		return err
	}
	store, err := openStore(filepath.Join(dir, "inbox.sqlite3"))
	if err != nil {
		return err
	}
	defer closeQuietly(store)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	w := &Worker{dir: dir, registry: registry, path: filepath.Join(sockets, fmt.Sprintf("%d.sock", pid)), control: filepath.Join(sockets, fmt.Sprintf("%d.control.sock", pid)), pid: pid, start: start, born: time.Now().UnixMilli(), config: cfg, store: store, ctx: ctx, cancel: cancel, lastTouch: time.Now(), artifacts: map[string]os.FileInfo{}, statusQueue: make(chan statusRequest, 128), drops: map[string][]Object{}, changed: make(chan struct{}, 1)}
	w.address = "uds:" + w.path
	defer w.cleanup()
	peer, err := w.listen(w.path)
	if err != nil {
		return err
	}
	defer closeQuietly(peer)
	control, err := w.listen(w.control)
	if err != nil {
		return err
	}
	defer closeQuietly(control)
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	keyFile := keyPath(registry, pid, w.path)
	if err = w.writeArtifact(keyFile, peerKey{Token: hex.EncodeToString(key), Start: start, Domain: runtime.GOOS}, 0600); err != nil {
		return err
	}
	w.setActivity("unknown", "No app-server observation yet")
	if err = w.registerName(); err != nil {
		return err
	}
	if err = w.writeArtifact(filepath.Join(dir, "worker.json"), workerMeta{PID: pid, Start: start, Control: w.control, Socket: w.path, Registry: registry, Thread: cfg.Thread, Key: keyFile, Runtime: "go"}, 0600); err != nil {
		return err
	}
	w.wg.Add(4)
	go func() { defer w.wg.Done(); w.serve(peer, false) }()
	go func() { defer w.wg.Done(); w.serve(control, true) }()
	go func() { defer w.wg.Done(); w.statusSender() }()
	go func() { defer w.wg.Done(); w.maintain() }()
	logEvent("ready", nil)
	<-ctx.Done()
	_ = peer.Close()
	_ = control.Close()
	w.wg.Wait()
	if w.client != nil {
		w.client.Close()
	}
	logEvent("stopped", nil)
	return nil
}
func (w *Worker) listen(path string) (*net.UnixListener, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("refusing to replace socket: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(false)
	if err = os.Chmod(path, 0600); err != nil {
		_ = l.Close()
		_ = os.Remove(path)
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	w.artifacts[path] = st
	return l, nil
}
func (w *Worker) writeArtifact(path string, value any, mode os.FileMode) error {
	w.artifactMu.Lock()
	defer w.artifactMu.Unlock()
	st, err := os.Lstat(path)
	if old, tracked := w.artifacts[path]; tracked {
		if err != nil || !os.SameFile(old, st) {
			return fmt.Errorf("worker artifact changed: %s", path)
		}
	} else if err == nil {
		return fmt.Errorf("refusing to replace existing artifact: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = atomicJSON(path, value, mode); err != nil {
		return err
	}
	st, err = os.Lstat(path)
	if err == nil {
		w.artifacts[path] = st
	}
	return err
}
func (w *Worker) cleanup() {
	for path, original := range w.artifacts {
		if st, err := os.Lstat(path); err == nil && os.SameFile(original, st) {
			_ = os.Remove(path)
		}
	}
}
func (w *Worker) snapshot() (Config, Object) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.config, clone(w.activity)
}
func (w *Worker) setActivity(state, detail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now().UnixMilli()
	updated := now
	if str(w.activity, "state") == state && str(w.activity, "detail") == detail {
		updated = int64(number(w.activity, "updated_at", float64(now)))
	}
	source := "app-server"
	if w.config.Delivery == "manual" {
		source = "unavailable"
	}
	w.activity = Object{"state": state, "source": source, "detail": detail, "observed_at": now, "updated_at": updated}
}
func (w *Worker) publish() error {
	cfg, activity := w.snapshot()
	status := "busy"
	if str(activity, "state") == "idle" {
		status = "idle"
	}
	return w.writeArtifact(filepath.Join(w.registry, fmt.Sprintf("%d.json", w.pid)), Object{"pid": w.pid, "sessionId": cfg.Thread, "hostPid": cfg.HostPID, "hostStart": cfg.HostStart, "cwd": cfg.CWD, "startedAt": w.born, "procStart": w.start, "version": Version, "peerProtocol": 1, "peerFeatures": []any{"inbox-receipts"}, "kind": "interactive", "entrypoint": "cli", "pidDomain": runtime.GOOS, "messagingSocketPath": w.path, "name": cfg.Name, "nameSource": "user", "nameSince": w.born, "status": status, "activity": activity, "updatedAt": time.Now().UnixMilli(), "statusUpdatedAt": activity["updated_at"]}, 0644)
}
func (w *Worker) info() (Object, error) {
	c, a := w.snapshot()
	unread, err := w.store.Count("unread")
	if err != nil {
		return nil, err
	}
	held, err := w.store.Count("held")
	if err != nil {
		return nil, err
	}
	status := "busy"
	if str(a, "state") == "idle" {
		status = "idle"
	}
	info := Object{"thread_id": c.Thread, "name": c.Name, "pid": w.pid, "address": w.address, "inbound": c.Inbound, "delivery": c.Delivery, "status": status, "activity": a, "permission_class": c.Permission, "unread": unread, "held": held, "runtime": "go", "version": Version, "app_server_socket": c.AppSocket}
	info["host_pid"], info["host_start"] = c.HostPID, c.HostStart
	for key, metaKey := range map[string]string{"notification": "wake", "delivery_error": "delivery_error", "delivery_waiting": "delivery_waiting"} {
		var value any
		if err = w.store.Meta(metaKey, &value); err != nil {
			return nil, err
		}
		info[key] = value
	}
	return info, nil
}
func (w *Worker) serve(listener *net.UnixListener, control bool) {
	limit := make(chan struct{}, 64)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if w.ctx.Err() == nil {
				logEvent("accept_failed", err)
				w.cancel()
			}
			return
		}
		select {
		case limit <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer func() { <-limit }()
			defer closeQuietly(conn)
			stop := context.AfterFunc(w.ctx, func() { _ = conn.Close() })
			defer stop()
			if control {
				w.controlConnection(conn)
			} else {
				w.peerConnection(conn)
			}
		}()
	}
}
func (w *Worker) peerConnection(conn *net.UnixConn) {
	pid, uid, err := peerCredentials(conn)
	if err != nil || uid != os.Getuid() {
		return
	}
	reader := bufio.NewReader(conn)
	start := ""
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		raw, err := readLine(reader, MaxBuffer)
		if err != nil {
			if !errors.Is(err, io.EOF) && w.ctx.Err() == nil {
				logEvent("peer_read_failed", err)
			}
			return
		}
		var frame Object
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		kind := str(frame, "type")
		if kind != "user" && kind != "control" {
			continue
		}
		if start == "" {
			start, err = processStart(pid)
			if err != nil {
				return
			}
		}
		if err = w.receive(frame, pid, start); err != nil {
			logEvent("peer_message_discarded", err)
		}
	}
}
func (w *Worker) admission(m Object) string {
	cfg, _ := w.snapshot()
	switch cfg.Inbound {
	case "accept":
		return "unread"
	case "hold":
		return "held"
	case "refuse":
		return "refused"
	}
	if str(m, "from_mode") == cfg.Permission || (cfg.Permission == "prompting" && m["from_mode"] == nil) {
		return "unread"
	}
	return "held"
}
func (w *Worker) receive(frame Object, pid int, start string) error {
	if str(frame, "type") == "control" {
		if str(frame, "action") == "peer_message_status" {
			return w.store.Correlate(frame, pid, start)
		}
		return nil
	}
	m, err := decodeUser(frame)
	if err != nil {
		return err
	}
	m["verified_peer_pid"] = pid
	m["verified_peer_start"] = start
	if pid == w.pid || strings.Contains(str(m, "hop_chain"), w.address) {
		w.drop(m, "relay_loop")
		return nil
	}
	state := w.admission(m)
	if state == "refused" {
		w.enqueueStatus(m, "refused", "", nil)
		return nil
	}
	accepted, reason, evicted, err := w.store.Accept(m, fmt.Sprintf("%d:%s", pid, start), state)
	if err != nil {
		return err
	}
	if reason != "" {
		w.drop(m, reason)
		return nil
	}
	if evicted != nil {
		w.enqueueStatus(evicted, "dropped", "full_queue", nil)
	}
	if state == "held" {
		w.enqueueStatus(accepted, "held", "", nil)
	} else {
		w.enqueueStatus(accepted, "accepted", "", nil)
		w.signal()
	}
	return nil
}
func (w *Worker) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}
func (w *Worker) drop(m Object, reason string) {
	w.dropMu.Lock()
	defer w.dropMu.Unlock()
	key := str(m, "from") + "\x00" + reason
	batch, ok := w.drops[key]
	if !ok && len(w.drops) >= 128 {
		return
	}
	if len(batch) < 100 {
		w.drops[key] = append(batch, m)
	}
}
func (w *Worker) flushDrops() {
	w.dropMu.Lock()
	batches := w.drops
	w.drops = map[string][]Object{}
	w.dropMu.Unlock()
	for key, batch := range batches {
		if len(batch) == 0 {
			continue
		}
		ids := []string{}
		for _, m := range batch[:len(batch)-1] {
			ids = append(ids, str(m, "wire_id"))
		}
		parts := strings.SplitN(key, "\x00", 2)
		w.enqueueStatus(batch[len(batch)-1], "dropped", parts[1], ids)
	}
}
func (w *Worker) enqueueStatus(m Object, status, reason string, ids []string) {
	select {
	case w.statusQueue <- statusRequest{m, status, reason, ids}:
	default:
		logEvent("status_queue_full", nil)
	}
}
func (w *Worker) statusSender() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case r := <-w.statusQueue:
			address := str(r.message, "from")
			if address == "" || address == w.address {
				continue
			}
			f := Object{"type": "control", "action": "peer_message_status", "status": r.status, "from": w.address, "orig_msg_id": r.message["wire_id"], "msgV": 1, "msg_id": uuid.NewString(), "reason": "Recipient inbox: " + r.status}
			if r.reason != "" {
				f["drop_reason"] = r.reason
				if r.ids == nil {
					r.ids = []string{}
				}
				f["dropped_msg_ids"] = r.ids
			}
			target, err := replyTarget(address, w.registry)
			if err == nil {
				if (r.status == "accepted" || r.status == "read") && !slices.Contains(target.Features, "inbox-receipts") {
					continue
				}
				err = sendFrame(target, f, w.registry)
			}
			if err != nil {
				logEvent("status_not_sent", err)
			}
		}
	}
}
func (w *Worker) maintain() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	next := time.Time{}
	for {
		if time.Now().After(next) {
			if err := w.maintenance(); err != nil {
				logEvent("maintenance_failed", err)
			}
			next = time.Now().Add(2 * time.Second)
		}
		select {
		case <-w.ctx.Done():
			return
		case <-tick.C:
			w.flushDrops()
		}
	}
}
func (w *Worker) maintenance() error {
	cfg, _ := w.snapshot()
	w.mu.RLock()
	last := w.lastTouch
	w.mu.RUnlock()
	if time.Since(last) > time.Duration(cfg.Lease)*time.Second {
		w.cancel()
		return nil
	}
	if err := cfg.checkHost(); err != nil {
		logEvent("client_owner_gone", err)
		w.cancel()
		return nil
	}
	old, err := w.store.Expire()
	if err != nil {
		return err
	}
	for _, m := range old {
		w.enqueueStatus(m, "expired", "", nil)
	}
	w.deliveryMu.Lock()
	defer w.deliveryMu.Unlock()
	cfg, _ = w.snapshot()
	if cfg.Delivery == "manual" {
		w.setActivity("unknown", "Manual delivery has no Codex activity connection")
		return w.publish()
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	if w.client == nil || w.client.Closed() {
		if w.client != nil {
			w.client.Close()
		}
		w.client, err = dialApp(ctx, cfg.AppSocket)
		w.attached = false
	}
	if err == nil {
		var thread appThread
		if !w.attached {
			thread, err = w.client.Attach(ctx, cfg.Thread)
			w.attached = err == nil
		} else {
			thread, err = w.client.ReadThread(ctx, cfg.Thread)
		}
		if err == nil {
			state := "idle"
			if thread.Status.Type == "active" {
				state = "busy"
			}
			w.setActivity(state, "")
		}
	}
	if err != nil {
		w.attached = false
		w.setActivity("unknown", err.Error())
		if e := w.store.SetMeta("delivery_error", err.Error()); e != nil {
			return e
		}
		return w.publish()
	}
	if err = w.publish(); err != nil {
		return err
	}
	return w.notify(ctx, cfg)
}
func (w *Worker) notify(ctx context.Context, cfg Config) error {
	n, err := w.store.Count("unread")
	if err != nil {
		return err
	}
	if n == 0 {
		return w.store.SetMeta("delivery_error", nil)
	}
	var wake Object
	if err = w.store.Meta("wake", &wake); err != nil {
		return err
	}
	if wake != nil {
		name := str(wake, "name")
		if name == "" {
			return nil
		} // Legacy uncertain notice: preserve until read/ack.
		w.client.Watch(cfg.Thread, name)
		if str(wake, "state") == "recorded" {
			return nil
		}
		if !w.client.Recorded() && time.Since(w.lastReconcile) < 15*time.Second {
			return nil
		}
		w.lastReconcile = time.Now()
		found, err := w.client.HasNotice(ctx, cfg.Thread, name)
		if err != nil {
			return w.store.SetMeta("delivery_error", err.Error())
		}
		if found {
			if err = w.store.FinishWake(str(wake, "id"), "recorded", "Codex recorded the function output; inbox acknowledgement is still required"); err != nil {
				return err
			}
			return w.store.SetMeta("delivery_error", nil)
		}
		return nil // Absence from history does not prove a previous write failed.
	}
	var retry float64
	if err = w.store.Meta("retry_at", &retry); err != nil {
		return err
	}
	if float64(time.Now().Unix()) < retry {
		return nil
	}
	wake, err = w.store.BeginWake(uuid.NewString())
	if err != nil || wake == nil {
		return err
	}
	id, name := str(wake, "id"), str(wake, "name")
	w.client.Watch(cfg.Thread, name)
	err = w.client.Notice(ctx, cfg.Thread, name, inboxNotice(cfg.Executable, cfg.Thread))
	if err != nil {
		state := "uncertain"
		var rejected *rpcError
		if errors.As(err, &rejected) {
			state = "rejected"
		}
		if e := w.store.FinishWake(id, state, err.Error()); e != nil {
			return e
		}
		if e := w.store.SetMeta("retry_at", time.Now().Add(30*time.Second).Unix()); e != nil {
			return e
		}
		return w.store.SetMeta("delivery_error", err.Error())
	}
	if err = w.store.FinishWake(id, "submitted", "App-server accepted the notice; model processing is not yet confirmed"); err != nil {
		return err
	}
	w.lastReconcile = time.Time{}
	return w.store.SetMeta("delivery_error", nil)
}
func (w *Worker) controlConnection(conn *net.UnixConn) {
	_, uid, err := peerCredentials(conn)
	if err != nil || uid != os.Getuid() {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	raw, err := readLine(bufio.NewReader(conn), MaxBuffer)
	var result Object
	if err == nil {
		var args Object
		err = json.Unmarshal(raw, &args)
		if err == nil {
			result, err = w.operation(args)
		}
	}
	response := Object{"result": result}
	if err != nil {
		response = Object{"error": err.Error()}
	}
	_, _ = conn.Write(append(compact(response), '\n'))
}
func inboxIDs(args Object) ([]string, error) {
	values, ok := args["ids"].([]any)
	if !ok || len(values) > 100 {
		return nil, errors.New("ids must be an array of up to 100 inbox IDs")
	}
	ids := []string{}
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("inbox IDs must be strings")
		}
		ids = append(ids, s)
	}
	return ids, nil
}
func (w *Worker) operation(args Object) (Object, error) {
	w.mu.Lock()
	w.lastTouch = time.Now()
	w.mu.Unlock()
	op := str(args, "op")
	switch op {
	case "info":
		return w.info()
	case "stop":
		time.AfterFunc(100*time.Millisecond, w.cancel)
		return Object{"stopping": true}, nil
	case "configure":
		return w.configure(args)
	case "read", "wait":
		state := str(args, "state")
		if state == "" {
			state = "unread"
		}
		switch state {
		case "unread", "held", "read", "declined", "expired":
		default:
			return nil, errors.New("invalid inbox state")
		}
		if op == "wait" {
			deadline := time.NewTimer(time.Duration(max(0, min(25, number(args, "timeout", 20))) * float64(time.Second)))
			defer deadline.Stop()
			for {
				n, err := w.store.Count(state)
				if err != nil {
					return nil, err
				}
				if n > 0 {
					break
				}
				select {
				case <-w.changed:
					continue
				case <-deadline.C:
					goto read
				case <-w.ctx.Done():
					return nil, w.ctx.Err()
				}
			}
		}
	read:
		info, err := w.info()
		if err != nil {
			return nil, err
		}
		return w.store.Page(state, str(args, "after"), max(1, min(50, int(number(args, "limit", 20)))), info)
	case "ack", "release", "decline":
		ids, err := inboxIDs(args)
		if err != nil {
			return nil, err
		}
		before, after, key := "held", "unread", "changed"
		switch op {
		case "ack":
			before = "unread"
			after = "read"
			key = "acknowledged"
		case "decline":
			after = "declined"
		}
		changed, err := w.store.Transition(ids, before, after)
		if err != nil {
			return nil, err
		}
		status := "released"
		switch op {
		case "decline":
			status = "declined"
		case "ack":
			status = "read"
		}
		for _, id := range changed {
			m, e := w.store.Get(id)
			if e != nil {
				return nil, e
			}
			w.enqueueStatus(m, status, "", nil)
		}
		w.signal()
		return Object{key: changed}, nil
	case "send", "reply":
		var target Peer
		var err error
		if op == "send" {
			target, err = resolve(str(args, "target"), w.registry, w.pid)
		} else {
			var m Object
			m, err = w.store.Get(str(args, "message_id"))
			if err == nil {
				if m == nil {
					return nil, errors.New("unknown inbox message")
				}
				target, err = replyTarget(str(m, "from"), w.registry)
			}
		}
		if err != nil {
			return nil, err
		}
		if target.PID == w.pid {
			return nil, errors.New("cannot message this same peer")
		}
		cfg, _ := w.snapshot()
		frame, err := userFrame(str(args, "body"), w.address, cfg.Name, cfg.Permission)
		if err != nil {
			return nil, err
		}
		if err = w.store.Outgoing(frame, target); err != nil {
			return nil, err
		}
		id := str(frame, "msg_id")
		if err = sendFrame(target, frame, w.registry); err != nil {
			if e := w.store.Sent(id, "failed", err.Error()); e != nil {
				return nil, errors.Join(err, e)
			}
			return nil, err
		}
		if err = w.store.Sent(id, "sent_unconfirmed", nil); err != nil {
			return nil, err
		}
		return Object{"message_id": id, "state": "sent_unconfirmed", "note": "Socket write succeeded. Recipient acceptance and model consumption are not confirmed."}, nil
	case "sent":
		messages, err := w.store.SentStatus(str(args, "message_id"))
		return Object{"messages": messages}, err
	default:
		return nil, errors.New("unknown bridge operation")
	}
}
func (w *Worker) configure(args Object) (Object, error) {
	w.deliveryMu.Lock()
	defer w.deliveryMu.Unlock()
	old, _ := w.snapshot()
	cfg := old
	options := clone(args)
	delete(options, "op")
	// Old hooks may still send activity. App-server observations stay authoritative.
	delete(options, "status")
	if err := applyConfig(&cfg, options); err != nil {
		return nil, err
	}
	if err := cfg.checkHost(); err != nil {
		return nil, err
	}
	if cfg.Name != old.Name {
		lock, err := lockPeerNames(w.registry)
		if err != nil {
			return nil, err
		}
		defer closeQuietly(lock)
		if err = checkPeerName(w.registry, cfg.Name, w.pid); err != nil {
			return nil, err
		}
	}
	changed := cfg.Delivery != old.Delivery || cfg.AppSocket != old.AppSocket
	if changed {
		if cfg.Delivery == "app-server" {
			ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
			defer cancel()
			client, err := dialApp(ctx, cfg.AppSocket)
			if err != nil {
				return nil, err
			}
			defer client.Close()
			if _, err = client.Attach(ctx, cfg.Thread); err != nil {
				return nil, err
			}
		}
		if err := w.store.ResetDelivery(); err != nil {
			return nil, err
		}
	}
	if err := atomicJSON(filepath.Join(w.dir, "config.json"), cfg, 0600); err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.config = cfg
	w.mu.Unlock()
	// Publish the new name while its claim is locked, before unrelated inbox
	// maintenance can fail. Roll back both persisted and advertised names on error.
	if err := w.publish(); err != nil {
		w.mu.Lock()
		w.config = old
		w.mu.Unlock()
		return nil, errors.Join(err, atomicJSON(filepath.Join(w.dir, "config.json"), old, 0600), w.publish())
	}
	if changed && w.client != nil {
		w.client.Close()
		w.client = nil
		w.attached = false
	}
	held, err := w.store.Read("held", 100)
	if err != nil {
		return nil, err
	}
	for _, m := range held {
		admission := w.admission(m)
		if admission == "unread" {
			n, e := w.store.Count("unread")
			if e != nil {
				return nil, e
			}
			if n >= 50 {
				break
			}
			if _, e = w.store.Transition([]string{str(m, "id")}, "held", "unread"); e != nil {
				return nil, e
			}
			w.enqueueStatus(m, "released", "", nil)
		} else if admission == "refused" {
			if _, e := w.store.Transition([]string{str(m, "id")}, "held", "declined"); e != nil {
				return nil, e
			}
			w.enqueueStatus(m, "refused", "", nil)
		}
	}
	w.signal()
	return w.info()
}

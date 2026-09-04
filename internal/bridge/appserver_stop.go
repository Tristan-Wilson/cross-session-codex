package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type appServerRecord struct {
	PID    int    `json:"pid"`
	Start  string `json:"proc_start"`
	Socket string `json:"socket"`
}

func appServerRecordPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "cross-session-codex-server.json")
}

// Shutdown only signals the kernel-verified server at the selected socket. It
// never searches for processes by name or escalates a timeout to SIGKILL.
func Shutdown(ctx context.Context, socket string, check bool) (Object, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, errors.New("app-server socket must be an absolute clean path")
	}
	lock, err := lockAppServer(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(lock)
	result := Object{"status": "not_running", "app_server_socket": socket}
	client, err := probeAppServer(ctx, socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ECONNREFUSED) {
			if err = checkStoppedAppServerRecord(socket); err != nil {
				return nil, err
			}
			return result, nil
		}
		return nil, fmt.Errorf("cannot verify app-server for shutdown: %w", err)
	}
	defer client.Close()
	identity, err := shutdownIdentity(ctx, socket, client.serverPID)
	if err != nil {
		return nil, err
	}
	before, err := owned(socket, os.ModeSocket, true)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(client.socketInfo, before) {
		return nil, errors.New("app-server socket changed after connecting; nothing was stopped")
	}
	owners, err := appServerOwners(socket)
	if err != nil {
		return nil, err
	}
	active, err := activeAppThreads(ctx, client)
	if err != nil {
		return nil, err
	}
	connections, err := appServerConnections(ctx, identity.PID, socket)
	if err != nil {
		return nil, err
	}
	result["pid"], result["sessions"], result["active_threads"], result["connections"] = identity.PID, owners, active, connections
	result["status"] = "ready"
	if len(owners) != 0 || len(active) != 0 || connections != 0 {
		result["status"] = "busy"
		if check {
			return result, nil
		}
		return nil, fmt.Errorf("refusing shutdown: %d live session owner(s), %d active thread(s), %d connected client(s); close all sessions, then retry (shutdown --check shows details)", len(owners), len(active), connections)
	}
	if check {
		return result, nil
	}
	// Recheck connections and process/socket identities immediately before the
	// signal. Cooperating launchers cannot attach while we hold the startup lock.
	if connections, err = appServerConnections(ctx, identity.PID, socket); err != nil {
		return nil, err
	} else if connections != 0 {
		return nil, errors.New("a client connected during shutdown; close all sessions and retry")
	}
	start, err := processStart(identity.PID)
	if err != nil || start != identity.Start {
		return nil, errors.New("app-server process identity changed; nothing was stopped")
	}
	after, err := owned(socket, os.ModeSocket, true)
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("app-server socket identity changed; nothing was stopped")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = unix.Kill(identity.PID, unix.SIGTERM); err != nil {
		return nil, fmt.Errorf("signal app-server: %w", err)
	}
	client.Close()
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stopCtx.Done():
			return nil, fmt.Errorf("app-server did not exit after SIGTERM: %w; no force kill was sent", stopCtx.Err())
		case <-ticker.C:
			start, err = processStart(identity.PID)
			if (err == nil && start != identity.Start) || errors.Is(unix.Kill(identity.PID, 0), unix.ESRCH) {
				result["status"] = "stopped"
				return result, nil
			}
		}
	}
}

func checkStoppedAppServerRecord(socket string) error {
	var recorded appServerRecord
	err := readJSON(appServerRecordPath(socket), true, &recorded)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if recorded.PID <= 1 || recorded.Start == "" || recorded.Socket != socket {
		return errors.New("invalid app-server launch record; cannot confirm shutdown")
	}
	start, err := processStart(recorded.PID)
	if (err == nil && start != recorded.Start) || errors.Is(unix.Kill(recorded.PID, 0), unix.ESRCH) {
		return nil
	}
	return errors.New("recorded app-server may still be alive but its socket is unavailable; cannot verify shutdown")
}

func shutdownIdentity(ctx context.Context, socket string, pid int) (appServerRecord, error) {
	identity := appServerRecord{PID: pid, Socket: socket}
	if pid <= 1 || pid == os.Getpid() {
		return identity, errors.New("refusing to signal an invalid or current process")
	}
	start, err := processStart(pid)
	if err != nil {
		return identity, err
	}
	identity.Start = start
	var recorded appServerRecord
	err = readJSON(appServerRecordPath(socket), true, &recorded)
	if err == nil {
		if recorded != identity {
			return identity, errors.New("app-server identity does not match the launch record; nothing was stopped")
		}
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return identity, err
	}
	// Older bridge versions did not record the PID. Recognize their exact direct
	// launch arguments, only after verifying the socket's kernel peer identity.
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "command=")
	b, err := cmd.Output()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(b)), " app-server --listen unix://"+socket) {
		return identity, errors.New("server has no matching bridge launch record or direct-launch command; nothing was stopped")
	}
	return identity, nil
}

func appServerOwners(socket string) ([]Object, error) {
	owners := []Object{}
	root := stateRoot()
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return owners, nil
	}
	if err := vetParents(root); err != nil {
		return nil, err
	}
	if _, err := owned(root, os.ModeDir, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if _, err = canonicalThread(entry.Name()); err != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err = owned(dir, os.ModeDir, true); err != nil {
			return nil, err
		}
		var cfg Config
		if err = readJSON(filepath.Join(dir, "config.json"), true, &cfg); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if cfg.AppSocket != socket || cfg.HostPID == 0 {
			continue
		}
		start, err := processStart(cfg.HostPID)
		if err != nil {
			if errors.Is(unix.Kill(cfg.HostPID, 0), unix.ESRCH) {
				continue
			}
			return nil, fmt.Errorf("cannot verify owner for thread %s: %w", entry.Name(), err)
		}
		if start == cfg.HostStart {
			owners = append(owners, Object{"thread_id": entry.Name(), "name": cfg.Name, "pid": cfg.HostPID})
		}
	}
	return owners, nil
}

func activeAppThreads(ctx context.Context, client *appClient) ([]string, error) {
	active := []string{}
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		params := Object{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data []string `json:"data"`
			Next string   `json:"nextCursor"`
		}
		if err := client.call(ctx, "thread/loaded/list", params, &page); err != nil {
			return nil, err
		}
		if page.Data == nil {
			return nil, errors.New("app-server omitted its loaded thread list; cannot verify shutdown")
		}
		for _, id := range page.Data {
			var response struct {
				Thread appThread `json:"thread"`
			}
			if err := client.call(ctx, "thread/read", Object{"threadId": id, "includeTurns": false}, &response); err != nil {
				return nil, err
			}
			if response.Thread.ID != id {
				return nil, errors.New("app-server returned a different thread during shutdown check")
			}
			switch response.Thread.Status.Type {
			case "idle", "notLoaded", "systemError":
			case "active":
				active = append(active, id)
			default:
				return nil, fmt.Errorf("cannot verify thread %s with status %q is idle", id, response.Thread.Status.Type)
			}
		}
		if page.Next == "" {
			return active, nil
		}
		if page.Next == cursor {
			return nil, errors.New("invalid app-server pagination")
		}
		cursor = page.Next
	}
	return nil, errors.New("app-server thread listing exceeded the shutdown check limit")
}

// Accepted Unix sockets retain the listener's pathname on both supported OSes.
// Counting them catches clients that have no messaging registration. The open,
// initialized inspection connection and listener account for two descriptors.
func appServerConnections(ctx context.Context, pid int, socket string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-U", "-F0pftn")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil || stderr.Len() != 0 {
		return 0, fmt.Errorf("cannot inspect app-server connections (shutdown requires lsof): %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return countAppConnections(b, pid, socket)
}

func countAppConnections(data []byte, pid int, socket string) (int, error) {
	process, kind, fd := "", "", ""
	seen := map[string]bool{}
	for _, raw := range bytes.Split(data, []byte{0}) {
		field := strings.TrimLeft(string(raw), "\n")
		if len(field) < 2 {
			continue
		}
		switch field[0] {
		case 'p':
			process, kind, fd = field[1:], "", ""
		case 'f':
			fd, kind = field[1:], ""
		case 't':
			kind = field[1:]
		case 'n':
			name := field[1:]
			if process == strconv.Itoa(pid) && kind == "unix" && fd != "" && (name == socket || strings.HasPrefix(name, socket+" type=STREAM")) {
				seen[fd] = true
			}
		}
	}
	if len(seen) < 2 {
		return 0, errors.New("could not verify the listener and inspection connection; nothing was stopped")
	}
	return len(seen) - 2, nil
}

package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Config struct {
	Thread     string `json:"thread_id"`
	CWD        string `json:"cwd"`
	Name       string `json:"name"`
	Inbound    string `json:"inbound"`
	Permission string `json:"permission_class"` // Messaging label, independent of Codex approvals.
	Delivery   string `json:"delivery"`
	AppSocket  string `json:"app_server_socket,omitempty"`
	Executable string `json:"executable,omitempty"`
	Lease      int    `json:"lease_seconds"`
	HostPID    int    `json:"host_pid,omitempty"`
	HostStart  string `json:"host_start,omitempty"`
}

func (c *Config) Validate() error {
	id, err := canonicalThread(c.Thread)
	if err != nil {
		return err
	}
	c.Thread = id
	switch c.Inbound {
	case "parity", "accept", "hold", "refuse":
	default:
		return errors.New("inbound must be parity, accept, hold or refuse")
	}
	if c.Permission != "prompting" && c.Permission != "bypass" {
		return errors.New("invalid permission class")
	}
	if c.Delivery != "app-server" && c.Delivery != "manual" {
		return errors.New("go delivery must be app-server or manual; use launch to replace a legacy tmux session")
	}
	c.Name = normalizeName(c.Name)
	if c.Name == "" {
		return errors.New("a nonempty peer name is required")
	}
	if c.Delivery == "app-server" && !filepath.IsAbs(c.AppSocket) {
		return errors.New("app-server socket must be an absolute local path")
	}
	if !filepath.IsAbs(c.CWD) {
		return errors.New("cwd must be absolute")
	}
	if c.Lease <= 0 {
		c.Lease = 86400
	}
	return nil
}
func stateRoot() string {
	return envOr("CROSS_SESSION_CODEX_STATE_DIR", filepath.Join(home(), ".local", "state", "cross-session-codex"))
}
func threadDir(thread string) (string, error) {
	id, err := canonicalThread(thread)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateRoot(), id), nil
}

type workerMeta struct {
	PID      int    `json:"pid"`
	Start    string `json:"procStart"`
	Control  string `json:"control"`
	Socket   string `json:"socket"`
	Registry string `json:"registry"`
	Thread   string `json:"thread_id"`
	Key      string `json:"key"`
	Runtime  string `json:"runtime,omitempty"`
}

func RPC(thread, op string, args Object) (Object, error) {
	dir, err := threadDir(thread)
	if err != nil {
		return nil, err
	}
	var m workerMeta
	if err = readJSON(filepath.Join(dir, "worker.json"), true, &m); err != nil {
		return nil, fmt.Errorf("messaging is unavailable for this thread; run start: %w", err)
	}
	if m.Thread != filepath.Base(dir) {
		return nil, errors.New("worker thread identity mismatch")
	}
	c, err := connectVerified(m.Control, m.PID, m.Start, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(c)
	request := clone(args)
	request["op"] = op
	b := append(compact(request), '\n')
	if len(b) > MaxBuffer {
		return nil, errors.New("control request exceeds size limit")
	}
	if _, err = c.Write(b); err != nil {
		return nil, err
	}
	raw, err := readLine(bufio.NewReader(c), MaxRPCResponse)
	if err != nil {
		return nil, err
	}
	var response struct {
		Result Object `json:"result"`
		Error  string `json:"error"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}
func recoverWorker(dir string) error {
	path := filepath.Join(dir, "worker.json")
	var m workerMeta
	if err := readJSON(path, true, &m); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	start, err := processStart(m.PID)
	if err == nil && start == m.Start {
		return errors.New("worker is alive but not responding; inspect worker.log")
	}
	aliveErr := unix.Kill(m.PID, 0)
	if err != nil && !errors.Is(aliveErr, unix.ESRCH) {
		return errors.New("worker cannot be verified; refusing to replace it")
	}
	if errors.Is(aliveErr, unix.ESRCH) {
		peer, err := socketPath("uds:" + m.Socket)
		if err != nil {
			return err
		}
		if filepath.Base(peer) != strconv.Itoa(m.PID)+".sock" {
			return errors.New("invalid stale worker socket")
		}
		for _, candidate := range []string{peer, filepath.Join(filepath.Dir(peer), fmt.Sprintf("%d.control.sock", m.PID)), keyPath(m.Registry, m.PID, peer), filepath.Join(m.Registry, fmt.Sprintf("%d.json", m.PID))} {
			kind := os.ModeSocket
			if filepath.Ext(candidate) != ".sock" {
				kind = 0
				var data Object
				if err = readJSON(candidate, false, &data); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return err
				}
				if str(data, "procStart") != m.Start {
					continue
				}
			}
			if _, err = owned(candidate, kind, false); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err = os.Remove(candidate); err != nil {
				return err
			}
		}
	}
	return os.Remove(path)
}
func Enable(thread string, options Object) (Object, error) {
	dir, err := threadDir(thread)
	if err != nil {
		return nil, err
	}
	if err = privateDir(stateRoot()); err != nil {
		return nil, err
	}
	if err = privateDir(dir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "start.lock")
	if _, err = os.Lstat(lockPath); err == nil {
		if _, err = owned(lockPath, 0, true); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, err
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	if info, e := RPC(thread, "info", nil); e == nil {
		if str(info, "runtime") != "go" {
			return nil, errors.New("a legacy worker is still running; disable it before starting the Go worker (inbox history is preserved)")
		}
		var existing Config
		if err = readJSON(filepath.Join(dir, "config.json"), true, &existing); err != nil {
			return nil, err
		}
		if err = applyConfig(&existing, options); err != nil {
			return nil, err
		}
		if err = existing.checkHost(); err != nil {
			return nil, err
		}
		if len(options) == 0 {
			return info, nil
		}
		return RPC(thread, "configure", options)
	}
	if err = recoverWorker(dir); err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	c := Config{Thread: filepath.Base(dir), CWD: cwd, Name: "codex-" + filepath.Base(cwd) + "-" + filepath.Base(dir)[:8], Inbound: "accept", Permission: "bypass", Delivery: "app-server", AppSocket: defaultAppSocket(), Lease: 86400}
	configPath := filepath.Join(dir, "config.json")
	if err = readJSON(configPath, true, &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Preserve a live launcher's owner when restarting its worker. A departed
	// owner is rejected below; launch supplies a new client identity explicitly.
	// Migrate legacy configuration, preserving names/admission and the SQLite file.
	if c.Delivery == "tmux" || c.Delivery == "queue" {
		c.Delivery = "app-server"
	}
	if c.AppSocket == "" {
		c.AppSocket = defaultAppSocket()
	}
	if err = applyConfig(&c, options); err != nil {
		return nil, err
	}
	c.Thread = filepath.Base(dir)
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	c.Executable = exe
	if err = c.Validate(); err != nil {
		return nil, err
	}
	if err = c.checkHost(); err != nil {
		return nil, err
	}
	if c.Delivery == "app-server" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, err := dialApp(ctx, c.AppSocket)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		if _, err = client.Attach(ctx, c.Thread); err != nil {
			return nil, err
		}
	}
	if err = atomicJSON(configPath, c, 0600); err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "worker.log")
	if _, err = os.Lstat(logPath); err == nil {
		if _, err = owned(logPath, 0, true); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	logFD, err := unix.Open(logPath, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	logfile := os.NewFile(uintptr(logFD), logPath)
	defer closeQuietly(logfile)
	cmd := exec.Command(exe, "worker", dir)
	cmd.Dir = c.CWD
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return nil, workerStartupError(err, logPath)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("worker startup timed out; see %s", logPath)
		case <-ticker.C:
			if info, err := RPC(thread, "info", nil); err == nil {
				return info, nil
			}
		}
	}
}

func workerStartupError(exitErr error, logPath string) error {
	message := ""
	fd, err := unix.Open(logPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		f := os.NewFile(uintptr(fd), logPath)
		defer closeQuietly(f)
		if st, err := f.Stat(); err == nil {
			if _, err = f.Seek(max(0, st.Size()-8192), io.SeekStart); err == nil {
				scanner := bufio.NewScanner(io.LimitReader(f, 8192))
				for scanner.Scan() {
					var entry Object
					if json.Unmarshal(scanner.Bytes(), &entry) == nil && str(entry, "error") != "" {
						message = str(entry, "error")
					}
				}
			}
		}
	}
	if message != "" {
		return fmt.Errorf("worker exited (%v): %s; see %s", exitErr, message, logPath)
	}
	return fmt.Errorf("worker exited (%v); see %s", exitErr, logPath)
}

func applyConfig(c *Config, args Object) error {
	for k, v := range args {
		switch k {
		case "name", "inbound", "permission_class", "delivery", "app_server_socket", "cwd":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s must be a string", k)
			}
			switch k {
			case "name":
				c.Name = s
			case "inbound":
				c.Inbound = s
			case "permission_class":
				c.Permission = s
			case "delivery":
				c.Delivery = s
			case "app_server_socket":
				c.AppSocket = s
			case "cwd":
				p, err := filepath.Abs(s)
				if err != nil {
					return err
				}
				c.CWD = p
			}
		case "host_pid":
			c.HostPID = int(number(args, k, 0))
		case "host_start":
			c.HostStart = str(args, k)
		default:
			return fmt.Errorf("unknown configuration option %q", k)
		}
	}
	return c.Validate()
}
func Stop(thread string) error {
	if _, err := RPC(thread, "stop", nil); err != nil {
		return err
	}
	dir, _ := threadDir(thread)
	for i := 0; i < 100; i++ {
		if _, err := os.Lstat(filepath.Join(dir, "worker.json")); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("worker is still shutting down; inspect status before restarting")
}

package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Start the selected CLI directly: Codex's daemon manager requires its separate
// standalone installer even when a compatible Homebrew or npm CLI is on PATH.
// The shared server survives this launcher so other attached UIs can keep using it.
func ensureAppServer(ctx context.Context, codex, socket, cwd string) (*appClient, error) {
	lock, err := lockAppServer(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(lock)
	return ensureAppServerLocked(ctx, codex, socket, cwd)
}

// Launch holds this lock until its client owner is registered and exec succeeds.
// Shutdown takes the same lock before checking owners or signalling the server.
func lockAppServer(ctx context.Context, socket string) (*os.File, error) {
	dir := filepath.Dir(socket)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	lock, err := openAppServerFile(filepath.Join(dir, "cross-session-codex-start.lock"))
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			closeQuietly(lock)
			return nil, err
		}
		select {
		case <-ctx.Done():
			closeQuietly(lock)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	return lock, nil
}

func ensureAppServerLocked(ctx context.Context, codex, socket, cwd string) (*appClient, error) {
	dir := filepath.Dir(socket)
	client, err := probeAppServer(ctx, socket)
	if err == nil {
		return client, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ECONNREFUSED) {
		return nil, err
	}
	logPath := filepath.Join(dir, "cross-session-codex.log")
	logfile, err := openAppServerFile(logPath)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(logfile)
	// Do not bind the shared process to the launcher's context or terminal.
	cmd := exec.Command(codex, "app-server", "--listen", "unix://"+socket)
	cmd.Dir = cwd
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return nil, fmt.Errorf("codex app-server exited (%v); see %s", err, logPath)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return nil, fmt.Errorf("codex app-server startup: %w; see %s", ctx.Err(), logPath)
		case <-ticker.C:
			if client, err = probeAppServer(ctx, socket); err == nil {
				start, e := processStart(cmd.Process.Pid)
				if e == nil && client.serverPID != cmd.Process.Pid {
					e = errors.New("app-server socket belongs to a different process")
				}
				if e == nil {
					e = atomicJSON(appServerRecordPath(socket), appServerRecord{PID: cmd.Process.Pid, Start: start, Socket: socket}, 0600)
				}
				if e != nil {
					client.Close()
					return nil, fmt.Errorf("record app-server identity: %w", e)
				}
				return client, nil
			}
		}
	}
}

func probeAppServer(ctx context.Context, socket string) (*appClient, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return dialApp(ctx, socket)
}

func openAppServerFile(path string) (*os.File, error) {
	if _, err := os.Lstat(path); err == nil {
		if _, err = owned(path, 0, true); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

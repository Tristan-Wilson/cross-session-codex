package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const launchHandshakeMaxFrame = 8192

var errLaunchHandshakeCanceled = errors.New("launch handoff canceled")

// LaunchReady describes the exact local registration, not a UI-attached receipt.
// Exchange fills the protocol version, event, and unpredictable launch nonce.
type LaunchReady struct {
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

type launchHandshake struct {
	readyFD, continueFD int
	closed              bool
}

// Both -1 descriptors mean no handoff was requested. Every other combination
// must supply two distinct pipe ends above stderr. Ownership transfers only
// after both descriptors have passed validation.
func newLaunchHandshake(readyFD, continueFD int) (*launchHandshake, error) {
	if readyFD == -1 && continueFD == -1 {
		return nil, nil
	}
	if readyFD < 3 || continueFD < 3 || readyFD == continueFD {
		return nil, errors.New("launch handoff requires distinct --ready-fd and --continue-fd pipe descriptors, both at least 3")
	}
	for _, endpoint := range []struct {
		fd, access int
		name       string
	}{
		{readyFD, unix.O_WRONLY, "ready"},
		{continueFD, unix.O_RDONLY, "continue"},
	} {
		var stat unix.Stat_t
		if err := unix.Fstat(endpoint.fd, &stat); err != nil {
			return nil, fmt.Errorf("launch handoff %s descriptor: %w", endpoint.name, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFIFO {
			return nil, fmt.Errorf("launch handoff %s descriptor must be a pipe", endpoint.name)
		}
		flags, err := unix.FcntlInt(uintptr(endpoint.fd), unix.F_GETFL, 0)
		if err != nil {
			return nil, fmt.Errorf("launch handoff %s descriptor flags: %w", endpoint.name, err)
		}
		if flags&unix.O_ACCMODE != endpoint.access {
			return nil, fmt.Errorf("launch handoff %s pipe has the wrong access mode", endpoint.name)
		}
	}
	h := &launchHandshake{readyFD: readyFD, continueFD: continueFD}
	for _, fd := range []int{readyFD, continueFD} {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err == nil {
			_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
		}
		if err == nil {
			err = unix.SetNonblock(fd, true)
		}
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("prepare launch handoff pipe: %w", err)
		}
	}
	return h, nil
}

// Close releases accepted descriptors. The caller must not use them afterward.
// It is safe for the launcher's deferred cleanup to call this after Exchange.
func (h *launchHandshake) Close() error {
	if h == nil || h.closed {
		return nil
	}
	h.closed = true
	return errors.Join(unix.Close(h.readyFD), unix.Close(h.continueFD))
}

// Exchange owns no goroutines and closes both descriptors on every outcome.
// The launcher supplies its fixed whole-exchange deadline through ctx.
func (h *launchHandshake) Exchange(ctx context.Context, ready LaunchReady) error {
	if h == nil {
		return nil
	}
	if h.closed {
		return errors.New("launch handoff is already closed")
	}
	defer closeQuietly(h)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("launch handoff: %w", err)
	}
	thread, err := canonicalThread(ready.ThreadID)
	if err != nil || thread != ready.ThreadID {
		return errors.New("launch handoff readiness requires an exact canonical thread UUID")
	}
	if !filepath.IsAbs(ready.CodexHome) || !filepath.IsAbs(ready.AppServerSocket) || ready.OwnerPID <= 0 || ready.OwnerStart == "" || ready.Name == "" || ready.Version == "" {
		return errors.New("launch handoff readiness requires absolute paths and complete owner, name, and version fields")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("launch handoff nonce: %w", err)
	}
	ready.V, ready.Event, ready.Nonce = 1, "ready", hex.EncodeToString(nonce[:])
	frame, err := json.Marshal(ready)
	if err != nil {
		return fmt.Errorf("encode launch readiness: %w", err)
	}
	frame = append(frame, '\n')
	if len(frame) > launchHandshakeMaxFrame {
		return errors.New("launch readiness frame exceeds 8192 bytes")
	}
	if err = writeHandshakeFrame(ctx, h.readyFD, frame); err != nil {
		return fmt.Errorf("write launch readiness: %w", err)
	}
	frame, err = readHandshakeFrame(ctx, h.continueFD)
	if err != nil {
		return fmt.Errorf("read launch continuation: %w", err)
	}
	if len(frame) == 0 {
		return errLaunchHandshakeCanceled
	}
	if bytes.IndexByte(frame, '\n') != len(frame)-1 {
		return errors.New("launch continuation must be one newline-terminated JSON frame followed by EOF")
	}
	decoder := json.NewDecoder(bytes.NewReader(frame[:len(frame)-1]))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("launch continuation must be a JSON object")
	}
	var v int
	var nonceText, action string
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid launch continuation: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("launch continuation contains an invalid or duplicate field")
		}
		seen[key] = true
		switch key {
		case "v":
			err = decoder.Decode(&v)
		case "nonce":
			err = decoder.Decode(&nonceText)
		case "action":
			err = decoder.Decode(&action)
		default:
			return errors.New("launch continuation contains an unknown field")
		}
		if err != nil {
			return fmt.Errorf("invalid launch continuation: %w", err)
		}
	}
	if _, err = decoder.Token(); err != nil {
		return fmt.Errorf("invalid launch continuation: %w", err)
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("launch continuation must contain exactly one JSON object")
	}
	if v != 1 || nonceText != ready.Nonce {
		return errors.New("launch continuation version or nonce does not match readiness")
	}
	switch action {
	case "continue":
		return nil
	case "cancel":
		return errLaunchHandshakeCanceled
	default:
		return errors.New("launch continuation action must be continue or cancel")
	}
}

func writeHandshakeFrame(ctx context.Context, fd int, frame []byte) error {
	for len(frame) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Write(fd, frame)
		if n > 0 {
			frame = frame[n:]
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			if err = waitHandshakePipe(ctx, fd, unix.POLLOUT); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readHandshakeFrame(ctx context.Context, fd int) ([]byte, error) {
	frame := make([]byte, 0, 256)
	var buffer [1024]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := unix.Read(fd, buffer[:])
		if n > 0 {
			if len(frame)+n > launchHandshakeMaxFrame {
				return nil, errors.New("launch continuation frame exceeds 8192 bytes")
			}
			frame = append(frame, buffer[:n]...)
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			if err = waitHandshakePipe(ctx, fd, unix.POLLIN); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return frame, nil
		}
	}
}

func waitHandshakePipe(ctx context.Context, fd int, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Short bounded polls make cancellation interrupt both a full ready pipe
		// and a parent that keeps the continuation pipe open, without goroutines.
		timeout := 25
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			if rounded := int((remaining + time.Millisecond - 1) / time.Millisecond); rounded < timeout {
				timeout = rounded
			}
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return unix.EBADF
		}
		if n > 0 {
			return nil // Read/write reports EOF, EPIPE, or the underlying error.
		}
	}
}

package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type handshakeFixture struct {
	h                        *launchHandshake
	readyRead, continueWrite int
}

func newHandshakeFixture(t *testing.T) *handshakeFixture {
	t.Helper()
	ready, continuation := make([]int, 2), make([]int, 2)
	must(t, unix.Pipe(ready))
	must(t, unix.Pipe(continuation))
	h, err := newLaunchHandshake(ready[1], continuation[0])
	must(t, err)
	f := &handshakeFixture{h: h, readyRead: ready[0], continueWrite: continuation[1]}
	t.Cleanup(func() {
		must(t, h.Close())
		must(t, unix.Close(f.readyRead))
		if f.continueWrite >= 0 {
			must(t, unix.Close(f.continueWrite))
		}
	})
	return f
}

func fixtureReady() LaunchReady {
	return LaunchReady{
		ThreadID: "11111111-1111-4111-8111-111111111111", CodexHome: "/private/codex",
		AppServerSocket: "/private/codex/app-server-control/app-server-control.sock",
		OwnerPID:        1234, OwnerStart: "Sun Sep 27 11:30:00 2026", Name: "local-peer", Version: Version,
	}
}

func (f *handshakeFixture) readiness(t *testing.T) LaunchReady {
	t.Helper()
	must(t, unix.SetNonblock(f.readyRead, true))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var frame []byte
	var buffer [1]byte
	for len(frame) <= launchHandshakeMaxFrame {
		n, err := unix.Read(f.readyRead, buffer[:])
		if errors.Is(err, unix.EAGAIN) {
			must(t, waitHandshakePipe(ctx, f.readyRead, unix.POLLIN))
			continue
		}
		must(t, err)
		if n == 0 {
			t.Fatal("ready pipe closed before the complete readiness frame")
		}
		frame = append(frame, buffer[0])
		if buffer[0] == '\n' {
			var ready LaunchReady
			must(t, json.Unmarshal(frame, &ready))
			return ready
		}
	}
	t.Fatal("oversized readiness")
	return LaunchReady{}
}

func (f *handshakeFixture) respond(t *testing.T, body string, closePipe bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	must(t, unix.SetNonblock(f.continueWrite, true))
	must(t, writeHandshakeFrame(ctx, f.continueWrite, []byte(body)))
	if closePipe {
		must(t, unix.Close(f.continueWrite))
		f.continueWrite = -1
	}
}

func continuationFrame(nonce, action string) string {
	return fmt.Sprintf("{\"v\":1,\"nonce\":%q,\"action\":%q}\n", nonce, action)
}

func TestLaunchHandshakeOptionalAndDescriptorValidation(t *testing.T) {
	h, err := newLaunchHandshake(-1, -1)
	must(t, err)
	if h != nil {
		t.Fatal("absent flags created a handoff")
	}
	must(t, h.Close())
	must(t, h.Exchange(context.Background(), LaunchReady{}))
	ready, continuation := make([]int, 2), make([]int, 2)
	must(t, unix.Pipe(ready))
	must(t, unix.Pipe(continuation))
	t.Cleanup(func() {
		for _, fd := range append(ready, continuation...) {
			must(t, unix.Close(fd))
		}
	})
	file, err := os.CreateTemp(t.TempDir(), "not-a-pipe")
	must(t, err)
	t.Cleanup(func() { must(t, file.Close()) })
	for _, test := range []struct {
		name string
		a, b int
	}{
		{"only ready", ready[1], -1}, {"only continue", -1, continuation[0]},
		{"stdin", 0, continuation[0]}, {"stdout", 1, continuation[0]},
		{"stderr", ready[1], 2}, {"same fd", ready[1], ready[1]},
		{"ready read end", ready[0], continuation[0]},
		{"continue write end", ready[1], continuation[1]},
		{"regular ready", int(file.Fd()), continuation[0]},
		{"regular continue", ready[1], int(file.Fd())},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := newLaunchHandshake(test.a, test.b); err == nil || got != nil {
				t.Fatalf("invalid descriptors accepted: handoff=%v err=%v", got, err)
			}
			for _, fd := range append(ready, continuation...) {
				_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
				must(t, err) // Validation failure must not acquire/close valid FDs.
				flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
				must(t, err)
				if flags&unix.O_NONBLOCK != 0 {
					t.Fatal("validation failure changed pipe status flags")
				}
			}
		})
	}
}

func TestLaunchHandshakeContinueAndOwnership(t *testing.T) {
	nonces := map[string]bool{}
	for range 2 {
		f := newHandshakeFixture(t)
		for _, fd := range []int{f.h.readyFD, f.h.continueFD} {
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			must(t, err)
			if flags&unix.FD_CLOEXEC == 0 {
				t.Fatal("handoff descriptor could survive exec")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		result := make(chan error, 1)
		go func() { result <- f.h.Exchange(ctx, fixtureReady()) }()
		ready := f.readiness(t)
		if ready.V != 1 || ready.Event != "ready" || len(ready.Nonce) != 32 || nonces[ready.Nonce] {
			t.Fatalf("invalid/reused readiness nonce: %+v", ready)
		}
		nonces[ready.Nonce] = true
		want := fixtureReady()
		want.V, want.Event, want.Nonce = ready.V, ready.Event, ready.Nonce
		if ready != want {
			t.Fatalf("readiness mismatch: got %+v want %+v", ready, want)
		}
		f.respond(t, continuationFrame(ready.Nonce, "continue"), true)
		must(t, <-result)
		cancel()
		for _, fd := range []int{f.h.readyFD, f.h.continueFD} {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
				t.Fatalf("handoff retained descriptor %d: %v", fd, err)
			}
		}
		must(t, f.h.Close())
		if err := f.h.Exchange(context.Background(), fixtureReady()); err == nil {
			t.Fatal("handoff was replayed")
		}
	}
}

func TestLaunchHandshakeRejectsInvalidContinuation(t *testing.T) {
	for _, test := range []struct {
		name string
		body func(string) string
	}{
		{"empty EOF", func(string) string { return "" }},
		{"cancel", func(n string) string { return continuationFrame(n, "cancel") }},
		{"bad nonce", func(string) string { return continuationFrame("wrong", "continue") }},
		{"bad action", func(n string) string { return continuationFrame(n, "resume") }},
		{"bad version", func(n string) string { return strings.Replace(continuationFrame(n, "continue"), `"v":1`, `"v":2`, 1) }},
		{"missing version", func(n string) string { return strings.Replace(continuationFrame(n, "continue"), `"v":1,`, "", 1) }},
		{"missing nonce", func(string) string { return "{\"v\":1,\"action\":\"continue\"}\n" }},
		{"missing action", func(n string) string { return fmt.Sprintf("{\"v\":1,\"nonce\":%q}\n", n) }},
		{"unknown field", func(n string) string {
			return strings.Replace(continuationFrame(n, "continue"), `"v":1`, `"v":1,"extra":true`, 1)
		}},
		{"duplicate field", func(n string) string {
			return strings.Replace(continuationFrame(n, "continue"), `"v":1`, `"v":1,"action":"cancel"`, 1)
		}},
		{"missing newline", func(n string) string { return strings.TrimSuffix(continuationFrame(n, "continue"), "\n") }},
		{"trailing bytes", func(n string) string { return continuationFrame(n, "continue") + " " }},
		{"second frame", func(n string) string { return continuationFrame(n, "continue") + continuationFrame(n, "continue") }},
		{"two JSON objects", func(n string) string { return "{}" + continuationFrame(n, "continue") }},
		{"null", func(string) string { return "null\n" }},
		{"array", func(string) string { return "[]\n" }},
		{"malformed JSON", func(string) string { return "{\n" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHandshakeFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- f.h.Exchange(ctx, fixtureReady()) }()
			ready := f.readiness(t)
			f.respond(t, test.body(ready.Nonce), true)
			err := <-result
			if err == nil {
				t.Fatal("invalid continuation was accepted")
			}
			if (test.name == "cancel" || test.name == "empty EOF") && !errors.Is(err, errLaunchHandshakeCanceled) {
				t.Fatalf("cancellation error=%v", err)
			}
		})
	}
}

func TestLaunchHandshakeContinuationRequiresEOF(t *testing.T) {
	f := newHandshakeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.h.Exchange(ctx, fixtureReady()) }()
	ready := f.readiness(t)
	f.respond(t, continuationFrame(ready.Nonce, "continue"), false)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent keeping continue pipe open did not time out: %v", err)
	}
}

func TestLaunchHandshakeBoundsBothFrames(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		f := newHandshakeFixture(t)
		ready := fixtureReady()
		ready.Name = strings.Repeat("x", launchHandshakeMaxFrame)
		if err := f.h.Exchange(context.Background(), ready); err == nil || !strings.Contains(err.Error(), "exceeds 8192") {
			t.Fatalf("oversized readiness accepted: %v", err)
		}
		buffer := make([]byte, 1)
		n, err := unix.Read(f.readyRead, buffer)
		must(t, err)
		if n != 0 {
			t.Fatal("oversized readiness was partially written")
		}
	})
	t.Run("continue", func(t *testing.T) {
		f := newHandshakeFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- f.h.Exchange(ctx, fixtureReady()) }()
		f.readiness(t)
		f.respond(t, strings.Repeat(" ", launchHandshakeMaxFrame)+"\n", true)
		if err := <-result; err == nil || !strings.Contains(err.Error(), "exceeds 8192") {
			t.Fatalf("oversized continuation accepted: %v", err)
		}
	})
}

func TestLaunchHandshakeContextInterruptsReadsAndWrites(t *testing.T) {
	for _, stage := range []string{"ready write", "continue read"} {
		for _, outcome := range []string{"timeout", "cancel"} {
			t.Run(stage+"/"+outcome, func(t *testing.T) {
				f := newHandshakeFixture(t)
				if stage == "ready write" {
					// Fill the ready pipe without reading it; Exchange must not block
					// forever even though no byte of its frame can be written.
					for {
						_, err := unix.Write(f.h.readyFD, bytes.Repeat([]byte("x"), 4096))
						if errors.Is(err, unix.EAGAIN) {
							break
						}
						must(t, err)
					}
				}
				var ctx context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if outcome == "timeout" {
					ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(context.Background())
					timer := time.AfterFunc(40*time.Millisecond, cancel)
					defer timer.Stop()
				}
				defer cancel()
				started := time.Now()
				err := f.h.Exchange(ctx, fixtureReady())
				if !errors.Is(err, want) {
					t.Fatalf("interruption=%v, want %v", err, want)
				}
				if time.Since(started) > time.Second {
					t.Fatal("blocked pipe did not promptly observe context")
				}
			})
		}
	}
}

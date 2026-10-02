package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type LaunchOptions struct {
	Resume, Name, Inbound, Permission, Socket, CWD, Codex string
	ClientArgs                                            []string
	ReadyFD, ContinueFD                                   int // -1 disables the optional handoff.
}

// A thread's saved Codex client owner stays authoritative after its messaging
// worker has stopped. Only a definitive owner exit lets launch claim the
// thread. An unverifiable owner is refused too, so a slow or failing ps can
// never produce a second Codex client on the same thread.
func refuseLiveOwner(thread string) error {
	dir, err := threadDir(thread)
	if err != nil {
		return err
	}
	var existing Config
	if err = readJSON(filepath.Join(dir, "config.json"), true, &existing); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if existing.HostPID <= 0 {
		return nil
	}
	err = existing.checkHost()
	switch {
	case err == nil:
		return fmt.Errorf("this thread already belongs to Codex client PID %d; exit its existing UI before resuming, or run cross-session-codex start --thread %s to restart its messaging worker", existing.HostPID, thread)
	case errors.Is(err, errOwnerGone):
		return nil
	}
	return fmt.Errorf("cannot verify whether Codex client PID %d still owns this thread: %w; retry, or exit that client first", existing.HostPID, err)
}

func Launch(opts LaunchOptions) error {
	if os.Getenv("CODEX_THREAD_ID") != "" {
		return errors.New("launch must run in your terminal after exiting Codex, not as a command inside an active Codex session")
	}
	handoff, err := newLaunchHandshake(opts.ReadyFD, opts.ContinueFD)
	if err != nil {
		return err
	}
	launchCtx := context.Background()
	stopSignals := func() {}
	if handoff != nil {
		defer closeQuietly(handoff)
	}
	if opts.Codex == "" {
		opts.Codex = "codex"
	}
	codex, err := exec.LookPath(opts.Codex)
	if err != nil {
		return err
	}
	codex, err = filepath.Abs(codex)
	if err != nil {
		return err
	}
	if opts.Socket == "" {
		opts.Socket = defaultAppSocket()
	}
	if opts.CWD == "" {
		opts.CWD, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	opts.CWD, err = filepath.Abs(opts.CWD)
	if err != nil {
		return err
	}
	if opts.Inbound != "" && opts.Inbound != "parity" && opts.Inbound != "accept" && opts.Inbound != "hold" && opts.Inbound != "refuse" {
		return errors.New("invalid inbound policy")
	}
	if opts.Permission != "" && opts.Permission != "bypass" && opts.Permission != "prompting" {
		return errors.New("invalid messaging permission class")
	}
	for _, arg := range opts.ClientArgs {
		for _, flag := range []string{"--remote", "--last", "--all", "--fork", "--cd", "-C"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return fmt.Errorf("launch owns thread routing and cwd; do not pass %s through to Codex", flag)
			}
		}
	}
	if opts.Resume != "" {
		opts.Resume, err = canonicalThread(opts.Resume)
		if err != nil {
			return err
		}
		// Check before touching the app-server, so a refused resume has no
		// side effects on the thread the live client is using.
		if err = refuseLiveOwner(opts.Resume); err != nil {
			return err
		}
	}
	if opts.Resume == "" && opts.Name != "" {
		// Fail before creating another conversation when its requested name is
		// already taken. The worker repeats this check under the registry lock.
		if err = checkPeerName(sessionsDir(), normalizeName(opts.Name), 0); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(launchCtx, 30*time.Second)
	defer cancel()
	lock, err := lockAppServer(ctx, opts.Socket)
	if err != nil {
		return err
	}
	defer closeQuietly(lock) // O_CLOEXEC releases it when the owning UI starts.
	if opts.Resume != "" {
		// Recheck under the lock. A concurrent launch may have installed a live
		// owner for this thread while this one waited.
		if err = refuseLiveOwner(opts.Resume); err != nil {
			return err
		}
	}
	var client *appClient
	if opts.Socket == defaultAppSocket() {
		client, err = ensureAppServerLocked(ctx, codex, opts.Socket, opts.CWD)
	} else {
		client, err = dialApp(ctx, opts.Socket)
	}
	if err != nil {
		return err
	}
	defer client.Close()
	var result struct {
		Thread appThread `json:"thread"`
	}
	if opts.Resume == "" {
		err = client.call(ctx, "thread/start", Object{"cwd": opts.CWD}, &result)
	} else {
		// Resuming is explicit here, unlike automatic worker attachment. The user
		// runs launch after leaving the prior embedded UI.
		err = client.call(ctx, "thread/resume", Object{"threadId": opts.Resume, "excludeTurns": true}, &result)
	}
	if err != nil {
		return err
	}
	thread, err := canonicalThread(result.Thread.ID)
	if err != nil {
		return err
	}
	if opts.Resume != "" && thread != opts.Resume {
		return errors.New("codex resumed a different thread")
	}
	if err = checkThread(result.Thread, thread); err != nil {
		return err
	}
	if opts.Resume == "" {
		if err = client.Bootstrap(ctx, thread); err != nil {
			return err
		}
	}
	if _, e := RPC(thread, "info", nil); e == nil {
		// The owner guard above already allowed this resume; only a worker whose
		// owner has departed can still be answering here.
		if err = Stop(thread); err != nil {
			return err
		}
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		return err
	}
	options := Object{"delivery": "app-server", "app_server_socket": opts.Socket, "cwd": result.Thread.CWD, "host_pid": os.Getpid(), "host_start": start}
	if opts.Name != "" {
		options["name"] = opts.Name
	}
	if opts.Inbound != "" {
		options["inbound"] = opts.Inbound
	}
	if opts.Permission != "" {
		options["permission_class"] = opts.Permission
	}
	state, err := Enable(thread, options)
	if err != nil {
		return err
	}
	// Successful exec never returns; any return after registration must remove
	// this provisional peer while preserving its durable thread and inbox.
	defer func() { _ = Stop(thread) }()
	if handoff != nil {
		// Preparation includes older blocking worker locks. Preserve normal
		// signal termination there; only the bounded handoff consumes signals.
		// Restore defaults before deferred worker cleanup, which may also wait
		// on a lock. Owner verification cleans up after an interrupted process.
		launchCtx, stopSignals = signal.NotifyContext(launchCtx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stopSignals()
	}
	_, _ = fmt.Fprintf(os.Stderr, "Cross Session Codex: %s (thread %s)\n", str(state, "name"), thread)
	client.Close()
	cancel()
	if handoff != nil {
		codexHome, err := filepath.Abs(envOr("CODEX_HOME", filepath.Join(home(), ".codex")))
		if err != nil {
			return err
		}
		socket, err := filepath.Abs(opts.Socket)
		if err != nil {
			return err
		}
		handoffCtx, cancelHandoff := context.WithTimeout(launchCtx, 30*time.Second)
		err = handoff.Exchange(handoffCtx, LaunchReady{
			ThreadID: thread, CodexHome: codexHome, AppServerSocket: socket,
			OwnerPID: os.Getpid(), OwnerStart: start, Name: str(state, "name"), Version: Version,
		})
		cancelHandoff()
		if err != nil {
			return fmt.Errorf("launch handoff: %w", err)
		}
		if err = launchCtx.Err(); err != nil {
			return fmt.Errorf("launch handoff canceled before exec: %w", err)
		}
	}
	if err = os.Chdir(opts.CWD); err != nil {
		return err
	}
	args := []string{codex, "--remote", "unix://" + opts.Socket, "resume", thread}
	args = append(args, opts.ClientArgs...)
	// Retain this PID across exec. The worker's host lease then ends when this
	// TUI exits, without a second supervisor or terminal automation process.
	stopSignals()
	if err = syscall.Exec(codex, args, os.Environ()); err != nil {
		return err
	}
	return nil
}

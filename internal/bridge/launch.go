package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type LaunchOptions struct {
	Resume, Name, Inbound, Permission, Socket, CWD, Codex string
	ClientArgs                                            []string
}

func Launch(opts LaunchOptions) error {
	if os.Getenv("CODEX_THREAD_ID") != "" {
		return errors.New("launch must run in your terminal after exiting Codex, not as a command inside an active Codex session")
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
	}
	if opts.Resume == "" && opts.Name != "" {
		// Fail before creating another conversation when its requested name is
		// already taken. The worker repeats this check under the registry lock.
		if err = checkPeerName(sessionsDir(), normalizeName(opts.Name), 0); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lock, err := lockAppServer(ctx, opts.Socket)
	if err != nil {
		return err
	}
	defer closeQuietly(lock) // O_CLOEXEC releases it when the owning UI starts.
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
		dir, e := threadDir(thread)
		if e != nil {
			return e
		}
		var existing Config
		if e = readJSON(filepath.Join(dir, "config.json"), true, &existing); e != nil {
			return e
		}
		if existing.HostPID > 0 && existing.checkHost() == nil {
			return fmt.Errorf("this thread already belongs to Codex client PID %d; exit its existing UI before resuming", existing.HostPID)
		}
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
	_, _ = fmt.Fprintf(os.Stderr, "Cross Session Codex: %s (thread %s)\n", str(state, "name"), thread)
	client.Close()
	cancel()
	if err = os.Chdir(opts.CWD); err != nil {
		_ = Stop(thread)
		return err
	}
	args := []string{codex, "--remote", "unix://" + opts.Socket, "resume", thread}
	args = append(args, opts.ClientArgs...)
	// Retain this PID across exec. The worker's host lease then ends when this
	// TUI exits, without a second supervisor or terminal automation process.
	if err = syscall.Exec(codex, args, os.Environ()); err != nil {
		_ = Stop(thread)
		return err
	}
	return nil
}

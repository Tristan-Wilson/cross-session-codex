package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A failed observation does not establish that a previously verified owner has
// exited. Callers establishing ownership must still reject an unknown result.
var (
	errHostUnverified = errors.New("codex client owner could not be verified")
	errOwnerGone      = errors.New("codex client owner has exited")
)

// A shared app-server is not a window's owner. Only launch can supply the
// client PID whose lifetime should control automatic message delivery.
func (c Config) checkHost() error {
	if c.HostPID == 0 && c.Delivery == "manual" {
		return nil
	}
	if c.HostPID <= 0 || c.HostStart == "" {
		return errors.New("automatic delivery requires a Codex client owner; exit the UI and use cross-session-codex launch --resume " + c.Thread)
	}
	start, err := processStart(c.HostPID)
	if err != nil {
		if errors.Is(unix.Kill(c.HostPID, 0), unix.ESRCH) {
			return fmt.Errorf("%w (PID %d); use launch --resume %s", errOwnerGone, c.HostPID, c.Thread)
		}
		return fmt.Errorf("%w: %v; retry the check", errHostUnverified, err)
	}
	if start != c.HostStart {
		return fmt.Errorf("%w (PID %d was reused); use launch --resume %s", errOwnerGone, c.HostPID, c.Thread)
	}
	return nil
}

// Serialize the name check and publication across starts and renames. Locking
// per thread is insufficient when two different threads request the same name.
func lockPeerNames(registry string) (*os.File, error) {
	f, err := openAppServerFile(filepath.Join(registry, ".cross-session-codex-names.lock"))
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		closeQuietly(f)
		return nil, err
	}
	return f, nil
}

func checkPeerName(registry, name string, exclude int) error {
	peers, err := Discover(registry, exclude)
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if normalizeName(peer.Name) == name {
			return fmt.Errorf("peer name %q is already registered by PID %d (ref %s); choose a different --name or close/disable that peer first", name, peer.PID, peer.Ref)
		}
	}
	return nil
}

func (w *Worker) registerName() error {
	lock, err := lockPeerNames(w.registry)
	if err != nil {
		return err
	}
	defer closeQuietly(lock)
	cfg, _ := w.snapshot()
	if err = checkPeerName(w.registry, cfg.Name, w.pid); err != nil {
		return err
	}
	return w.publish()
}

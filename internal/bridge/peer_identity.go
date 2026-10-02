package bridge

import (
	"errors"
	"runtime"
)

// Peer identity formats are protocol-specific. Keep processStart and persisted
// worker/client ownership in their legacy UTC-ps format; only peer endpoints
// may advertise a platform-specific alternative.
func verifyPeerIdentity(pid int, start, domain string) error {
	if pid <= 0 || start == "" {
		return errors.New("invalid peer process identity")
	}
	if domain == runtime.GOOS {
		actual, err := processStart(pid)
		if err != nil {
			return err
		}
		if actual != start {
			return errors.New("stale process identity")
		}
		return nil
	}
	return verifyPlatformPeerIdentity(pid, start, domain)
}

// A receipt's observed start was obtained from the kernel-identified socket
// sender, not from its message. Numeric peer starts must describe that same
// still-live process before they can correlate an outgoing message.
func peerReceiptMatches(target Peer, pid int, observedStart string) bool {
	if target.PID != pid {
		return false
	}
	if target.Domain == "" || target.Domain == runtime.GOOS {
		// Preserve legacy outbound rows, including rows without a domain.
		// New endpoint discovery/dialing requires a domain; old receipt rows do not.
		return target.Start == observedStart
	}
	actual, err := processStart(pid)
	if err != nil || actual != observedStart {
		return false
	}
	return verifyPlatformPeerIdentity(pid, target.Start, target.Domain) == nil
}

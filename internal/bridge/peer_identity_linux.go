package bridge

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var linuxPeerDomainRE = regexp.MustCompile(`^linux:([0-9a-f]{32}):(pid:\[[1-9][0-9]*\])$`)
var processTicksRE = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func verifyPlatformPeerIdentity(pid int, start, domain string) error {
	return verifyLinuxPeerIdentity(pid, start, domain, os.ReadFile, os.Readlink)
}

// readFile/readlink are injected only by fixtures; live callers always read
// kernel process metadata and the local machine ID, never peer-provided paths.
func verifyLinuxPeerIdentity(pid int, start, domain string, readFile func(string) ([]byte, error), readlink func(string) (string, error)) error {
	parts := linuxPeerDomainRE.FindStringSubmatch(domain)
	if pid <= 0 || len(parts) != 3 || !canonicalProcessTicks(start) {
		return errors.New("unsupported peer process identity format")
	}
	machine, err := readFile("/etc/machine-id")
	if err != nil {
		return fmt.Errorf("read peer machine identity: %w", err)
	}
	if strings.TrimSpace(string(machine)) != parts[1] {
		return errors.New("peer machine identity mismatch")
	}
	selfNS, err := readlink("/proc/self/ns/pid")
	if err != nil {
		return fmt.Errorf("read local PID namespace: %w", err)
	}
	peerNS, err := readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return fmt.Errorf("read peer PID namespace: %w", err)
	}
	if selfNS != parts[2] || peerNS != selfNS {
		return errors.New("peer PID namespace mismatch")
	}
	stat, err := readFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return fmt.Errorf("read peer process start: %w", err)
	}
	actual, err := parseLinuxProcessStart(stat, pid)
	if err != nil {
		return err
	}
	if actual != start {
		return errors.New("stale process identity")
	}
	return nil
}

func canonicalProcessTicks(start string) bool {
	if !processTicksRE.MatchString(start) {
		return false
	}
	_, err := strconv.ParseUint(start, 10, 64)
	return err == nil
}

func parseLinuxProcessStart(body []byte, pid int) (string, error) {
	text := string(body)
	open, closeParen := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if pid <= 0 || open < 1 || closeParen <= open || strings.TrimSpace(text[:open]) != strconv.Itoa(pid) {
		return "", errors.New("malformed peer process stat")
	}
	// comm (field 2) may contain spaces, newlines, and closing parentheses.
	// The final ')' terminates it; starttime is field 22, or index 19 here.
	fields := strings.Fields(text[closeParen+1:])
	if len(fields) < 20 || len(fields[0]) != 1 || !canonicalProcessTicks(fields[19]) {
		return "", errors.New("malformed peer process start ticks")
	}
	// Zombies and dead tasks cannot own a live messaging endpoint. Unknown
	// states also fail closed instead of treating future formats as evidence.
	if !strings.Contains("RSDTtWKPI", fields[0]) {
		return "", errors.New("peer process is not live or its state is unknown")
	}
	return fields[19], nil
}

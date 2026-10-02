package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const fixtureMachineID = "0123456789abcdef0123456789abcdef"
const fixturePIDNamespace = "pid:[123456]"

func fixtureProcessStat(pid int, comm, ticks string) []byte {
	return []byte(fmt.Sprintf("%d (%s) S %s%s 0 0\n", pid, comm, strings.Repeat("0 ", 18), ticks))
}

func TestLinuxPeerProcessStatParsing(t *testing.T) {
	for _, comm := range []string{"program", "with spaces", "ends)", "((a) b))", "name\nwith\nlines"} {
		t.Run(comm, func(t *testing.T) {
			start, err := parseLinuxProcessStart(fixtureProcessStat(42, comm, "12345"), 42)
			if err != nil || start != "12345" {
				t.Fatalf("parsed start=%q err=%v", start, err)
			}
		})
	}
	for _, body := range [][]byte{
		[]byte("42 no-parentheses S 0"), []byte("42 (program) S 0"),
		fixtureProcessStat(43, "wrong PID", "12345"),
		fixtureProcessStat(42, "program", "01"), fixtureProcessStat(42, "program", "-1"),
		fixtureProcessStat(42, "program", "1.2"), fixtureProcessStat(42, "program", "18446744073709551616"),
	} {
		if _, err := parseLinuxProcessStart(body, 42); err == nil {
			t.Fatalf("accepted malformed process stat %q", body)
		}
	}
	for _, state := range []string{"Z", "X", "x", "?", "RUNNING"} {
		body := strings.Replace(string(fixtureProcessStat(42, "program", "12345")), ") S ", ") "+state+" ", 1)
		if _, err := parseLinuxProcessStart([]byte(body), 42); err == nil {
			t.Fatalf("accepted dead or malformed process state %q", state)
		}
	}
}

func TestLinuxPeerIdentityRejectsUnknownOrStaleEvidence(t *testing.T) {
	domain := "linux:" + fixtureMachineID + ":" + fixturePIDNamespace
	cases := []struct {
		name, start, domain, machine, selfNS, peerNS, stat   string
		readFailure, linkFailure, statFailure, peerNSFailure bool
	}{
		{name: "valid"},
		{name: "wrong ticks", start: "12346"},
		{name: "leading zero ticks", start: "012345"},
		{name: "signed ticks", start: "+12345"},
		{name: "overflow ticks", start: "18446744073709551616"},
		{name: "whitespace ticks", start: "12345 "},
		{name: "date with numeric domain", start: "Sat Jan  1 00:00:00 2000"},
		{name: "wrong machine", machine: strings.Repeat("f", 32)},
		{name: "wrong caller namespace", selfNS: "pid:[123457]"},
		{name: "wrong target namespace", peerNS: "pid:[123457]"},
		{name: "unknown domain", domain: "darwin"},
		{name: "missing machine domain", domain: "linux::" + fixturePIDNamespace},
		{name: "noncanonical namespace", domain: "linux:" + fixtureMachineID + ":pid:[0123456]"},
		{name: "trailing domain", domain: domain + ":extra"},
		{name: "reused process", stat: string(fixtureProcessStat(42, "replacement", "12346"))},
		{name: "wrong stat PID", stat: string(fixtureProcessStat(43, "program", "12345"))},
		{name: "unreadable metadata", readFailure: true},
		{name: "unreadable stat", statFailure: true},
		{name: "unreadable namespace", linkFailure: true},
		{name: "unreadable target namespace", peerNSFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, advertisedDomain := tc.start, tc.domain
			if start == "" {
				start = "12345"
			}
			if advertisedDomain == "" {
				advertisedDomain = domain
			}
			machine, selfNS, peerNS, stat := tc.machine, tc.selfNS, tc.peerNS, tc.stat
			if machine == "" {
				machine = fixtureMachineID + "\n"
			}
			if selfNS == "" {
				selfNS = fixturePIDNamespace
			}
			if peerNS == "" {
				peerNS = fixturePIDNamespace
			}
			if stat == "" {
				stat = string(fixtureProcessStat(42, "program", "12345"))
			}
			readFile := func(path string) ([]byte, error) {
				if tc.readFailure {
					return nil, os.ErrPermission
				}
				switch path {
				case "/etc/machine-id":
					return []byte(machine), nil
				case "/proc/42/stat":
					if tc.statFailure {
						return nil, os.ErrPermission
					}
					return []byte(stat), nil
				default:
					t.Fatalf("unexpected metadata path %q", path)
					return nil, os.ErrNotExist
				}
			}
			readlink := func(path string) (string, error) {
				if tc.linkFailure {
					return "", os.ErrPermission
				}
				switch path {
				case "/proc/self/ns/pid":
					return selfNS, nil
				case "/proc/42/ns/pid":
					if tc.peerNSFailure {
						return "", os.ErrPermission
					}
					return peerNS, nil
				default:
					t.Fatalf("unexpected namespace path %q", path)
					return "", os.ErrNotExist
				}
			}
			err := verifyLinuxPeerIdentity(42, start, advertisedDomain, readFile, readlink)
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("identity validation error=%v", err)
			}
		})
	}
}

func currentNativePeerIdentity(t *testing.T) (string, string) {
	t.Helper()
	machine, err := os.ReadFile("/etc/machine-id")
	must(t, err)
	namespace, err := os.Readlink("/proc/self/ns/pid")
	must(t, err)
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", os.Getpid()))
	must(t, err)
	start, err := parseLinuxProcessStart(stat, os.Getpid())
	must(t, err)
	return start, "linux:" + strings.TrimSpace(string(machine)) + ":" + namespace
}

func TestLinuxNativePeerDiscoverySendReplyAndReceipt(t *testing.T) {
	isolatedState(t)
	registry := sessionsDir()
	must(t, privateDir(registry))
	sockets, err := chooseSocketDir()
	must(t, err)
	path := filepath.Join(sockets, strconv.Itoa(os.Getpid())+".sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	must(t, err)
	defer closeQuietly(listener)
	must(t, os.Chmod(path, 0600))
	start, domain := currentNativePeerIdentity(t)
	native := Peer{PID: os.Getpid(), Start: start, Domain: domain, Socket: path,
		Name: "native-fixture", Protocol: 1, Features: []string{"inbox-receipts"}}
	advertisement := filepath.Join(registry, strconv.Itoa(native.PID)+".json")
	must(t, atomicJSON(advertisement, native, 0644))
	// Fixture key only: never inspect another running session's credentials.
	key := peerKey{Token: strings.Repeat("0", 32), Start: start, Domain: domain}
	must(t, atomicJSON(keyPath(registry, native.PID, path), key, 0600))
	frames := make(chan Object, 16)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(conn)
			for {
				line, err := readLine(reader, MaxBuffer)
				if err != nil {
					break
				}
				var frame Object
				if json.Unmarshal(line, &frame) == nil && str(frame, "type") == "user" {
					select {
					case frames <- frame:
					case <-ctx.Done():
					}
				}
			}
			closeQuietly(conn)
		}
	}()
	defer func() {
		cancel()
		closeQuietly(listener)
		<-done
	}()
	waitUser := func(want string) Object {
		t.Helper()
		select {
		case frame := <-frames:
			decoded, err := decodeUser(frame)
			must(t, err)
			if str(decoded, "body") != want {
				t.Fatalf("received body %q, want %q", str(decoded, "body"), want)
			}
			return frame
		case <-time.After(5 * time.Second):
			t.Fatal("native fixture received no user frame")
			return nil
		}
	}
	thread := uuid.NewString()
	startTestPeer(t, thread, "legacy-fixture")
	peers, err := Discover(registry, 0)
	must(t, err)
	if len(peers) != 2 {
		t.Fatalf("native/legacy discovery count=%d, want 2", len(peers))
	}
	peer, err := resolve(native.Name, registry, 0)
	must(t, err)
	if peer.Start != start || peer.Domain != domain || peer.Ref == "" {
		t.Fatal("native identity lost during discovery")
	}
	cli(t, "send", peer.Ref, "--thread", thread, "--body", "native discovery send")
	message := waitUser("native discovery send")
	legacy, err := resolve("legacy-fixture", registry, 0)
	must(t, err)
	receipt := Object{"type": "control", "action": "peer_message_status", "status": "read", "orig_msg_id": message["msg_id"]}
	must(t, sendFrame(legacy, receipt, registry))
	eventually(t, func() bool {
		result, err := RPC(thread, "sent", Object{"message_id": message["msg_id"]})
		if err != nil {
			return false
		}
		rows := result["messages"].([]any)
		return len(rows) == 1 && str(rows[0].(map[string]any), "state") == "read"
	})
	incoming, err := userFrame("native request", "uds:"+path, native.Name, "bypass")
	must(t, err)
	must(t, sendFrame(legacy, incoming, registry))
	got := cli(t, "wait", "--thread", thread, "--timeout", "5")
	messages := got["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("native request count=%d, want 1", len(messages))
	}
	cli(t, "reply", str(messages[0].(map[string]any), "id"), "--thread", thread, "--body", "native reply")
	waitUser("native reply")

	for _, mutation := range []string{"start", "domain"} {
		bad := native
		if mutation == "start" {
			bad.Start += "0"
		} else {
			bad.Domain = "linux:" + fixtureMachineID + ":pid:[1]"
		}
		must(t, atomicJSON(advertisement, bad, 0644))
		if _, err := resolve(native.Name, registry, 0); err == nil {
			t.Fatalf("discovered peer with mismatched %s", mutation)
		}
		if conn, err := connectVerifiedPeer(path, bad.PID, bad.Start, bad.Domain, time.Second); err == nil {
			closeQuietly(conn)
			t.Fatalf("connected to peer with mismatched %s", mutation)
		}
		if err := sendFrame(bad, incoming, registry); err == nil {
			t.Fatalf("accepted key/advertisement %s mismatch", mutation)
		}
	}
	// An inconsistent advertisement may not grant optional reply capabilities.
	resolved, err := replyTarget("uds:"+path, registry)
	must(t, err)
	if len(resolved.Features) != 0 {
		t.Fatal("reply adopted capabilities from a foreign-domain advertisement")
	}
	// The advertised process is live, but a different process owns this
	// listening socket. Post-connect kernel credentials must reject it.
	parentStat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", os.Getppid()))
	must(t, err)
	parentStart, err := parseLinuxProcessStart(parentStat, os.Getppid())
	must(t, err)
	if conn, err := connectVerifiedPeer(path, os.Getppid(), parentStart, domain, time.Second); err == nil {
		closeQuietly(conn)
		t.Fatal("socket owned by a different live process was accepted")
	}
}

func TestLinuxNativeReceiptIdentity(t *testing.T) {
	start, domain := currentNativePeerIdentity(t)
	observed, err := processStart(os.Getpid())
	must(t, err)
	target := Peer{PID: os.Getpid(), Start: start, Domain: domain}
	if !peerReceiptMatches(target, target.PID, observed) {
		t.Fatal("same live native sender did not match")
	}
	if peerReceiptMatches(target, target.PID, "different process") || peerReceiptMatches(target, target.PID+1, observed) {
		t.Fatal("receipt matched a different observed process")
	}
	target.Start += "0"
	if peerReceiptMatches(target, target.PID, observed) {
		t.Fatal("receipt matched reused PID with different start ticks")
	}
	target.Start, target.Domain = start, "linux:"+fixtureMachineID+":pid:[1]"
	if peerReceiptMatches(target, target.PID, observed) {
		t.Fatal("receipt matched a foreign process domain")
	}
}

func TestLinuxNativePeerRequiresReadableCurrentMetadata(t *testing.T) {
	start, domain := currentNativePeerIdentity(t)
	must(t, verifyPeerIdentity(os.Getpid(), start, domain))
	for _, invalid := range []string{"", "linux:unknown", "darwin", "unknown"} {
		if err := verifyPeerIdentity(os.Getpid(), start, invalid); err == nil {
			t.Fatalf("accepted domain %q", invalid)
		}
	}
	if err := verifyLinuxPeerIdentity(os.Getpid(), start, domain,
		func(string) ([]byte, error) { return nil, os.ErrPermission }, os.Readlink); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable process evidence err=%v", err)
	}
}

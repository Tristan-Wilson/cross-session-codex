// Package bridge implements a local, same-user Claude-compatible message peer.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	MaxBuffer         = 1 << 20
	MaxSerialized     = 1_000_000
	MaxRPCResponse    = 55 * MaxBuffer
	MaxReadResponse   = 8 * MaxBuffer
	closingTag        = "</cross-session-message>"
	escapedClosingTag = `<\/cross-session-message>`
)

var Version = "0.2.0-dev"

type Object = map[string]any

// Cleanup failures cannot usefully change an already committed result. Writes,
// flushes and commits are checked at their call sites before cleanup runs.
func closeQuietly(c io.Closer) { _ = c.Close() }

// compact uses UTF-8 JSON, matching the peer protocol's byte and UTF-16 limits.
func compact(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		panic(err)
	} // Only JSON-compatible internal values.
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}
func str(m Object, key string) string { s, _ := m[key].(string); return s }
func number(m Object, key string, fallback float64) float64 {
	switch n := m[key].(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return fallback
}
func clone(m Object) Object {
	n := Object{}
	for k, v := range m {
		n[k] = v
	}
	return n
}
func canonicalThread(s string) (string, error) {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		return "", errors.New("an explicit current Codex thread UUID is required")
	}
	return id.String(), nil
}
func processStart(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid process id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return "", fmt.Errorf("cannot verify process %d: %v", pid, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func owned(path string, kind os.FileMode, private bool) (os.FileInfo, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	var raw unix.Stat_t
	if err = unix.Lstat(path, &raw); err != nil {
		return nil, err
	}
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	if st.Mode().Type() != kind || raw.Uid != uint32(os.Getuid()) || st.Mode().Perm()&mask != 0 {
		return nil, fmt.Errorf("unsafe type, owner or permissions: %s", path)
	}
	return st, nil
}
func vetParents(path string) error {
	if !filepath.IsAbs(path) || strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") {
		return errors.New("path must be absolute without '..'")
	}
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		var st unix.Stat_t
		if err := unix.Lstat(cur, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil || cur != "/tmp" || resolved != "/private/tmp" || st.Uid != 0 {
				return fmt.Errorf("symlink component: %s", cur)
			}
			if err = unix.Stat(cur, &st); err != nil {
				return err
			}
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && st.Uid != uint32(os.Getuid())) || (st.Mode&0022 != 0 && st.Mode&unix.S_ISVTX == 0) {
			return fmt.Errorf("untrusted directory: %s", cur)
		}
	}
	return nil
}
func privateDir(path string) error {
	ancestor := path
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return err
		}
		ancestor = parent
	}
	if err := vetParents(ancestor); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := vetParents(path); err != nil {
		return err
	}
	_, err := owned(path, os.ModeDir, true)
	return err
}
func readJSON(path string, private bool, out any) error {
	if _, err := owned(path, 0, private); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer closeQuietly(f)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	if st.Uid != uint32(os.Getuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || os.FileMode(st.Mode)&mask != 0 || st.Size > MaxBuffer {
		return errors.New("invalid JSON artifact")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBuffer+1))
	if err != nil {
		return err
	}
	if len(b) > MaxBuffer {
		return errors.New("JSON artifact too large")
	}
	return json.Unmarshal(b, out)
}
func atomicJSON(path string, v any, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer closeQuietly(f)
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(compact(v)); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func home() string { h, _ := os.UserHomeDir(); return h }
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func sessionsDir() string {
	return filepath.Join(envOr("CLAUDE_CONFIG_DIR", filepath.Join(home(), ".claude")), "sessions")
}

var socketDirRE = regexp.MustCompile(`^(/(?:private/)?tmp/cc-socks(?:-(?:0|[1-9]\d*))?|/run/user/(?:0|[1-9]\d*)/cc-socks|/data/data/com\.termux/files/usr/tmp/cc-socks(?:-(?:0|[1-9]\d*))?)$`)
var socketNameRE = regexp.MustCompile(`^[1-9]\d*\.sock$`)
var tokenRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func chooseSocketDir() (string, error) {
	for _, p := range []string{filepath.Join(envOr("XDG_RUNTIME_DIR", "/tmp"), "cc-socks"), fmt.Sprintf("/tmp/cc-socks-%d", os.Getuid())} {
		if socketDirRE.MatchString(p) && privateDir(p) == nil {
			return p, nil
		}
	}
	return "", errors.New("no safe Claude-compatible socket directory")
}
func socketPath(address string) (string, error) {
	p := strings.TrimPrefix(address, "uds:")
	if p == address || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, " \t\n\r") || !socketDirRE.MatchString(filepath.Dir(p)) || !socketNameRE.MatchString(filepath.Base(p)) {
		return "", errors.New("not a Claude-compatible local uds: peer address")
	}
	return p, nil
}
func keyPath(registry string, pid int, path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(registry, fmt.Sprintf("%d.%x.key", pid, sum))
}
func peerCredentials(conn *net.UnixConn) (pid, uid int, err error) {
	raw, e := conn.SyscallConn()
	if e != nil {
		return 0, 0, e
	}
	err = raw.Control(func(fd uintptr) { pid, uid, e = socketCredentials(int(fd)) })
	if err == nil {
		err = e
	}
	return
}
func connectVerified(path string, pid int, start string, timeout time.Duration) (*net.UnixConn, error) {
	if err := vetParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := owned(filepath.Dir(path), os.ModeDir, true); err != nil {
		return nil, err
	}
	before, err := owned(path, os.ModeSocket, true)
	if err != nil {
		return nil, err
	}
	actual, err := processStart(pid)
	if err != nil {
		return nil, err
	}
	if actual != start {
		return nil, errors.New("stale process identity")
	}
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	conn := c.(*net.UnixConn)
	valid := false
	defer func() {
		if !valid {
			closeQuietly(conn)
		}
	}()
	if err = conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	p, u, err := peerCredentials(conn)
	if err != nil {
		return nil, err
	}
	after, err := owned(path, os.ModeSocket, true)
	if err != nil {
		return nil, err
	}
	actual, err = processStart(p)
	if err != nil {
		return nil, err
	}
	if u != os.Getuid() || p != pid || actual != start || !os.SameFile(before, after) {
		return nil, errors.New("connected endpoint identity mismatch")
	}
	valid = true
	return conn, nil
}

type Peer struct {
	PID       int      `json:"pid"`
	Start     string   `json:"procStart"`
	Domain    string   `json:"pidDomain"`
	Socket    string   `json:"messagingSocketPath"`
	Name      string   `json:"name,omitempty"`
	Thread    string   `json:"sessionId,omitempty"`
	HostPID   int      `json:"hostPid,omitempty"`
	HostStart string   `json:"hostStart,omitempty"`
	Features  []string `json:"peerFeatures,omitempty"`
	Ref       string   `json:"ref,omitempty"`
	CWD       string   `json:"cwd,omitempty"`
	Status    string   `json:"status,omitempty"`
	Activity  Object   `json:"activity,omitempty"`
	Version   string   `json:"version,omitempty"`
	Protocol  int      `json:"peerProtocol,omitempty"`
}
type peerKey struct {
	Token  string `json:"peerToken"`
	Start  string `json:"procStart"`
	Domain string `json:"pidDomain"`
}

func Discover(registry string, exclude int) ([]Peer, error) {
	if registry == "" {
		registry = sessionsDir()
	}
	peers := []Peer{}
	if _, err := os.Stat(registry); errors.Is(err, os.ErrNotExist) {
		return peers, nil
	}
	if err := vetParents(registry); err != nil {
		return nil, err
	}
	if _, err := owned(registry, os.ModeDir, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(registry)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var p Peer
		if readJSON(filepath.Join(registry, e.Name()), false, &p) != nil || strconv.Itoa(p.PID)+".json" != e.Name() || p.PID == exclude || p.Protocol != 1 || p.Name == "" {
			continue
		}
		start, err := processStart(p.PID)
		if err != nil || start != p.Start {
			continue
		}
		if p.HostPID > 0 {
			ownerStart, err := processStart(p.HostPID)
			if err != nil || ownerStart != p.HostStart {
				continue
			}
		}
		if _, err = socketPath("uds:" + p.Socket); err != nil {
			continue
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", p.PID, p.Start)))
		p.Ref = hex.EncodeToString(h[:])[:10]
		peers = append(peers, p)
	}
	return peers, nil
}
func resolve(target, registry string, exclude int) (Peer, error) {
	peers, err := Discover(registry, exclude)
	if err != nil {
		return Peer{}, err
	}
	var matches []Peer
	for _, p := range peers {
		if target == p.Name || target == p.Ref || target == "uds:"+p.Socket {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return Peer{}, errors.New("target is missing or ambiguous; list sessions and use its current ref")
	}
	return matches[0], nil
}
func normalizeName(name string) string {
	r := []rune(strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cs, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, name)))
	if len(r) > 64 {
		return string(r[:63]) + "…"
	}
	return string(r)
}
func userFrame(body, address, name, mode string) (Object, error) {
	if body == "" || !utf8.ValidString(body) {
		return nil, errors.New("body must be nonempty UTF-8 text")
	}
	if mode != "prompting" && mode != "bypass" {
		return nil, errors.New("unknown permission class")
	}
	envelope := fmt.Sprintf("<cross-session-message from=\"%s\" from-name=\"%s\" from-mode=\"%s\">\n%s\n%s", html.EscapeString(address), html.EscapeString(normalizeName(name)), mode, strings.ReplaceAll(body, closingTag, escapedClosingTag), closingTag)
	f := Object{"msgV": 1, "msg_id": uuid.NewString(), "type": "user", "message": Object{"role": "user", "content": envelope}, "priority": "next", "from": address}
	b := compact(f)
	if len(b)+1 > MaxBuffer || len(utf16.Encode([]rune(string(b)))) > MaxSerialized {
		return nil, errors.New("message exceeds the peer size limit")
	}
	return f, nil
}

var envelopeRE = regexp.MustCompile(`(?s)^<cross-session-message\s+([^>]+)>\n(.*)\n</cross-session-message>$`)
var attrRE = regexp.MustCompile(`([\w-]+)="([^"]*)"`)

func decodeUser(frame Object) (Object, error) {
	if number(frame, "msgV", 0) != 1 || str(frame, "type") != "user" {
		return nil, errors.New("unsupported user frame")
	}
	if _, err := uuid.Parse(str(frame, "msg_id")); err != nil {
		return nil, errors.New("invalid message UUID")
	}
	m, ok := frame["message"].(map[string]any)
	if !ok || str(m, "role") != "user" {
		return nil, errors.New("invalid user frame")
	}
	text, ok := m["content"].(string)
	if !ok {
		return nil, errors.New("invalid user content")
	}
	body := text
	attrs := map[string]string{}
	if match := envelopeRE.FindStringSubmatch(text); match != nil {
		for _, a := range attrRE.FindAllStringSubmatch(match[1], -1) {
			attrs[a[1]] = html.UnescapeString(a[2])
		}
		body = strings.ReplaceAll(match[2], escapedClosingTag, closingTag)
	}
	var mode any
	if v, ok := attrs["from-mode"]; ok {
		mode = v
	}
	return Object{"wire_id": frame["msg_id"], "body": body, "from": frame["from"], "from_name": normalizeName(attrs["from-name"]), "from_mode": mode, "raw_envelope": text, "hop_chain": attrs["hop-chain"]}, nil
}
func replyTarget(address, registry string) (Peer, error) {
	path, err := socketPath(address)
	if err != nil {
		return Peer{}, err
	}
	pid, _ := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".sock"))
	var key peerKey
	if err = readJSON(keyPath(registry, pid, path), true, &key); err != nil {
		return Peer{}, err
	}
	peer := Peer{PID: pid, Start: key.Start, Domain: key.Domain, Socket: path}
	var advertised Peer
	if readJSON(filepath.Join(registry, strconv.Itoa(pid)+".json"), false, &advertised) == nil && advertised.PID == pid && advertised.Start == key.Start && advertised.Socket == path {
		peer.Features = advertised.Features
	}
	return peer, nil
}
func sendFrame(target Peer, frame Object, registry string) error {
	path, err := socketPath("uds:" + target.Socket)
	if err != nil {
		return err
	}
	var key peerKey
	if err = readJSON(keyPath(registry, target.PID, path), true, &key); err != nil {
		return err
	}
	if key.Start != target.Start || key.Domain != target.Domain || !tokenRE.MatchString(key.Token) {
		return errors.New("target key identity mismatch")
	}
	payload := append(append(append(compact(Object{"type": "auth", "token": key.Token}), '\n'), compact(frame)...), '\n')
	if len(payload) > MaxBuffer {
		return errors.New("serialized payload exceeds the buffer cap")
	}
	conn, err := connectVerified(path, target.PID, target.Start, 3*time.Second)
	if err != nil {
		return err
	}
	defer closeQuietly(conn)
	_, err = io.Copy(conn, bytes.NewReader(payload))
	return err
}
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var all []byte
	for {
		b, err := r.ReadSlice('\n')
		if len(all)+len(b) > max {
			return nil, errors.New("frame exceeds size limit")
		}
		all = append(all, b...)
		if err == nil {
			return all, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

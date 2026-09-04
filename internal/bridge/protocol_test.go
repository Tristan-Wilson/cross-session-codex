package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testDir(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	dir, err := os.MkdirTemp(base, "csc-go-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func TestEnvelopeRoundTrip(t *testing.T) {
	for _, body := range []string{"Hello\n世界 🧪", `literal </cross-session-message> and backslash \ keep`, `already <\/cross-session-message>`, "line one\n\"quoted\"\nline three"} {
		f, err := userFrame(body, "uds:/tmp/cc-socks/123.sock", "a\x00\u200bb\n\"<name>", "prompting")
		must(t, err)
		var wire Object
		must(t, json.Unmarshal(compact(f), &wire))
		m, err := decodeUser(wire)
		must(t, err)
		want := strings.ReplaceAll(body, escapedClosingTag, closingTag)
		if str(m, "body") != want {
			t.Fatalf("body changed: %q != %q", m["body"], want)
		}
		if str(m, "from_name") != "ab\"<name>" {
			t.Fatalf("name was not normalized: %q", m["from_name"])
		}
		if !strings.Contains(str(m, "raw_envelope"), "cross-session-message") {
			t.Fatal("missing raw envelope")
		}
	}
}
func TestProtocolBoundsAndUnrecognizedEnvelope(t *testing.T) {
	for _, body := range []string{strings.Repeat("a", MaxSerialized), strings.Repeat("🧪", MaxBuffer/4), string([]byte{0xff}), ""} {
		if _, err := userFrame(body, "uds:/tmp/cc-socks/123.sock", "test", "prompting"); err == nil {
			t.Fatal("oversized/invalid body accepted")
		}
	}
	f := Object{"msgV": 1, "msg_id": uuid.NewString(), "type": "user", "message": Object{"role": "user", "content": `no envelope <\/cross-session-message>`}}
	m, err := decodeUser(f)
	must(t, err)
	if str(m, "body") != `no envelope <\/cross-session-message>` {
		t.Fatal("unrecognized envelope was normalized")
	}
	f["msg_id"] = "bad"
	if _, err = decodeUser(f); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if len([]rune(normalizeName(strings.Repeat("x", 80)))) != 64 {
		t.Fatal("name length not bounded")
	}
	if _, err = readLine(bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxBuffer)+"\n")), MaxBuffer); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
func TestSocketAddressValidation(t *testing.T) {
	for _, address := range []string{"uds:/tmp/cc-socks/123.sock", "uds:/private/tmp/cc-socks-501/9.sock", "uds:/run/user/1000/cc-socks/2.sock"} {
		if _, err := socketPath(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"tcp:localhost:123", "uds:/tmp/evil.sock", "uds:/tmp/cc-socks/1.control.sock", "uds:/tmp/cc-socks/../cc-socks/1.sock", "uds:/tmp/cc-socks/0.sock", "uds:/tmp/cc-socks/01.sock", "uds:/tmp/cc-socks//2.sock", "uds:/tmp/cc-socks/1.sock\n"} {
		if _, err := socketPath(address); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
}
func TestPrivatePathsAndSymlinks(t *testing.T) {
	dir := testDir(t)
	must(t, privateDir(filepath.Join(dir, "safe")))
	unsafe := filepath.Join(dir, "unsafe")
	must(t, os.Mkdir(unsafe, 0777))
	must(t, os.Chmod(unsafe, 0777))
	if err := privateDir(unsafe); err == nil {
		t.Fatal("writable directory accepted")
	}
	link := filepath.Join(dir, "link")
	must(t, os.Symlink(filepath.Join(dir, "safe"), link))
	if err := privateDir(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink parent accepted")
	}
	file := filepath.Join(dir, "data.json")
	must(t, atomicJSON(file, Object{"ok": true}, 0600))
	must(t, os.Symlink(file, filepath.Join(dir, "data-link")))
	var result Object
	if err := readJSON(filepath.Join(dir, "data-link"), true, &result); err == nil {
		t.Fatal("symlink JSON accepted")
	}
}
func TestVerifiedUnixCredentials(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	must(t, err)
	defer closeQuietly(listener)
	must(t, os.Chmod(path, 0600))
	start, err := processStart(os.Getpid())
	must(t, err)
	done := make(chan error, 1)
	go func() {
		c, e := listener.AcceptUnix()
		if e != nil {
			done <- e
			return
		}
		defer closeQuietly(c)
		p, u, e := peerCredentials(c)
		if e == nil && (p != os.Getpid() || u != os.Getuid()) {
			e = os.ErrPermission
		}
		done <- e
	}()
	c, err := connectVerified(path, os.Getpid(), start, time.Second)
	must(t, err)
	closeQuietly(c)
	must(t, <-done)
	if _, err = connectVerified(path, os.Getpid(), "stale", time.Second); err == nil {
		t.Fatal("stale identity accepted")
	}
	must(t, os.Chmod(path, 0666))
	if _, err = connectVerified(path, os.Getpid(), start, time.Second); err == nil {
		t.Fatal("writable socket accepted")
	}
}
func TestShellQuoteAndCLIBodyParsing(t *testing.T) {
	quoted := shellQuote("/tmp/a'b $(nope) `never`")
	if quoted != "'/tmp/a'\"'\"'b $(nope) `never`'" {
		t.Fatalf("bad quoting: %s", quoted)
	}
	var out, errOut bytes.Buffer
	if code := Main([]string{"send", "target", "--body", "--literal body", "--thread", "invalid"}, strings.NewReader(""), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "UUID") {
		t.Fatalf("body incorrectly parsed: %s", errOut.String())
	}
}

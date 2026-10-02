package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const connectionUnixHeader = "Num RefCount Protocol Flags Type St Inode Path\n"

func connectionUnixRow(inode int, flags, state, path string) string {
	return fmt.Sprintf("0000000000000000: 00000003 00000000 %s 0001 %s %d%s\n", flags, state, inode, path)
}

func connectionProcFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "123")
	for _, dir := range []string{"fd", "ns", "net"} {
		must(t, os.MkdirAll(filepath.Join(base, dir), 0700))
	}
	stat := "123 (fixture process) S " + strings.Repeat("0 ", 18) + "99\n"
	must(t, os.WriteFile(filepath.Join(base, "stat"), []byte(stat), 0600))
	must(t, os.Symlink("net:[100]", filepath.Join(base, "ns", "net")))
	must(t, os.Symlink("socket:[10]", filepath.Join(base, "fd", "3")))
	must(t, os.Symlink("socket:[11]", filepath.Join(base, "fd", "4")))
	socket := "/tmp/example directory/app.sock "
	table := connectionUnixHeader + connectionUnixRow(10, "00010000", "01", " "+socket) +
		connectionUnixRow(11, "00000000", "03", " "+socket)
	must(t, os.WriteFile(filepath.Join(base, "net", "unix"), []byte(table), 0600))
	return root, base, socket
}

func TestLinuxConnectionProcInventory(t *testing.T) {
	for _, count := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root, base, socket := connectionProcFixture(t)
			path := filepath.Join(base, "net", "unix")
			data, err := os.ReadFile(path)
			must(t, err)
			for i := range count {
				inode := 20 + i
				must(t, os.Symlink(fmt.Sprintf("socket:[%d]", inode), filepath.Join(base, "fd", fmt.Sprint(6+i))))
				data = append(data, connectionUnixRow(inode, "00000000", "03", " "+socket)...)
			}
			// Duplicates must not inflate the listener or connection counts.
			must(t, os.Symlink("socket:[10]", filepath.Join(base, "fd", "40")))
			must(t, os.Symlink("socket:[11]", filepath.Join(base, "fd", "41")))
			must(t, os.Symlink("socket:[90]", filepath.Join(base, "fd", "42")))
			data = append(data, connectionUnixRow(90, "00010000", "01", " "+socket+".other")...)
			must(t, os.WriteFile(path, data, 0600))
			got, err := linuxAppServerConnections(context.Background(), root, 123, socket, readConnectionProc)
			must(t, err)
			if got != count {
				t.Fatalf("got %d connections, want %d", got, count)
			}
		})
	}
}

func TestLinuxConnectionProcRefusesIncompleteEvidence(t *testing.T) {
	for _, mutation := range []string{"missing-listener", "missing-probe", "duplicate-listener-only", "unknown-inode", "malformed-link", "malformed-stat", "unreadable-fd", "missing-unix", "duplicate-inode", "truncated-unix", "bad-header", "bad-row", "wrong-type", "wrong-state"} {
		t.Run(mutation, func(t *testing.T) {
			root, base, socket := connectionProcFixture(t)
			path := filepath.Join(base, "net", "unix")
			data, err := os.ReadFile(path)
			must(t, err)
			switch mutation {
			case "missing-listener":
				must(t, os.Remove(filepath.Join(base, "fd", "3")))
			case "missing-probe", "duplicate-listener-only":
				must(t, os.Remove(filepath.Join(base, "fd", "4")))
				if mutation == "duplicate-listener-only" {
					must(t, os.Symlink("socket:[10]", filepath.Join(base, "fd", "5")))
				}
			case "unknown-inode", "malformed-link":
				target := "socket:[999]"
				if mutation == "malformed-link" {
					target = "socket:[invalid]"
				}
				must(t, os.Symlink(target, filepath.Join(base, "fd", "5")))
			case "malformed-stat":
				must(t, os.WriteFile(filepath.Join(base, "stat"), []byte("123 (broken) S\n"), 0600))
			case "unreadable-fd":
				must(t, os.WriteFile(filepath.Join(base, "fd", "5"), nil, 0600))
			case "missing-unix":
				must(t, os.Remove(path))
			case "duplicate-inode":
				data = append(data, connectionUnixRow(10, "00010000", "01", " "+socket)...)
			case "truncated-unix":
				data = data[:len(data)-1]
			case "bad-header":
				data = []byte(strings.Replace(string(data), "Type St", "St Type", 1))
			case "bad-row":
				data = append(data, "not a socket row\n"...)
			case "wrong-type":
				data = []byte(strings.ReplaceAll(string(data), " 0001 ", " 0002 "))
			case "wrong-state":
				data = []byte(strings.ReplaceAll(string(data), " 03 ", " 02 "))
			}
			if mutation != "missing-unix" {
				must(t, os.WriteFile(path, data, 0600))
			}
			if _, err = linuxAppServerConnections(context.Background(), root, 123, socket, readConnectionProc); err == nil {
				t.Fatal("accepted incomplete connection evidence")
			}
		})
	}
}

func TestLinuxConnectionProcRejectsChurnAndCancellation(t *testing.T) {
	for _, mutation := range []string{"fd", "identity", "namespace", "socket-state", "canceled", "unreadable"} {
		t.Run(mutation, func(t *testing.T) {
			root, base, socket := connectionProcFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			read := func(ctx context.Context, path string) ([]byte, error) {
				data, err := readConnectionProc(ctx, path)
				if path == filepath.Join(base, "net", "unix") {
					reads++
					if reads == 1 {
						switch mutation {
						case "fd":
							must(t, os.Symlink("/tmp/new-file", filepath.Join(base, "fd", "9")))
						case "identity":
							stat, e := os.ReadFile(filepath.Join(base, "stat"))
							must(t, e)
							must(t, os.WriteFile(filepath.Join(base, "stat"), []byte(strings.Replace(string(stat), "99", "100", 1)), 0600))
						case "namespace":
							must(t, os.Remove(filepath.Join(base, "ns", "net")))
							must(t, os.Symlink("net:[101]", filepath.Join(base, "ns", "net")))
						case "socket-state":
							must(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(data), " 03 ", " 02 ")), 0600))
						case "canceled":
							cancel()
						case "unreadable":
							return nil, os.ErrPermission
						}
					}
				}
				return data, err
			}
			if _, err := linuxAppServerConnections(ctx, root, 123, socket, read); err == nil {
				t.Fatal("accepted changing or unreadable evidence")
			}
		})
	}
}

func TestLinuxConnectionOtherTables(t *testing.T) {
	for _, protocol := range []string{"tcp", "tcp6", "udp", "udp6", "netlink"} {
		t.Run(protocol, func(t *testing.T) {
			root, base, socket := connectionProcFixture(t)
			must(t, os.Symlink("socket:[30]", filepath.Join(base, "fd", "9")))
			header := "sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode"
			address := "00000000:0000"
			if strings.HasSuffix(protocol, "6") {
				header = strings.Replace(header, "rem_address", "remote_address", 1)
				address = strings.Repeat("0", 32) + ":0000"
			}
			row := "0: " + address + " " + address + " 0A 00000000:00000000 00:00000000 00000000 1000 0 30 1 0000000000000000"
			if strings.HasPrefix(protocol, "udp") {
				header += " ref pointer drops"
				row += " 0"
			}
			if protocol == "netlink" {
				header = "sk Eth Pid Groups Rmem Wmem Dump Locks Drops Inode"
				row = "0000000000000000 0 123 00000000 0 0 0 2 0 30"
			}
			data := []byte(header + "\n" + row + "\n")
			path := filepath.Join(base, "net", protocol)
			must(t, os.WriteFile(path, data, 0600))
			got, err := linuxAppServerConnections(context.Background(), root, 123, socket, readConnectionProc)
			must(t, err)
			if got != 0 {
				t.Fatalf("non-Unix socket counted: %d", got)
			}
			for _, broken := range [][]byte{data[:len(data)-1], []byte(header + "\ninvalid row\n"), []byte("wrong header\n"), []byte(header + "\n" + strings.Replace(row, "0000000000000000", "nothex", 1) + "\n")} {
				if _, err = parseConnectionOther(broken, protocol); err == nil {
					t.Fatal("accepted malformed non-Unix table")
				}
			}
		})
	}
}

func TestLinuxConnectionProcReadBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized")
	must(t, os.WriteFile(path, bytes.Repeat([]byte{'x'}, connectionProcLimit+1), 0600))
	if _, err := readConnectionProc(context.Background(), path); err == nil {
		t.Fatal("accepted oversized proc file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readConnectionProc(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

// A disposable child owns both sides' server descriptors. Its stdin EOF ends
// the fixture; no installed binary, model, credentials, or live session is used.
func TestLinuxConnectionInventoryProcess(t *testing.T) {
	path := os.Getenv("CSC_TEST_CONNECTION_SOCKET")
	if path == "" {
		t.Skip("connection inventory subprocess fixture")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	must(t, err)
	defer closeQuietly(listener)
	duplicate, err := listener.File()
	must(t, err)
	defer closeQuietly(duplicate)
	other, err := net.Listen("unix", path+".other")
	must(t, err)
	defer closeQuietly(other)
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
	defer closeQuietly(tcp)
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	must(t, err)
	defer closeQuietly(udp)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	must(t, err)
	defer func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) }()
	for _, server := range []net.Listener{listener, other, tcp} {
		go func() {
			for {
				conn, e := server.Accept()
				if e != nil {
					return
				}
				go func() {
					defer closeQuietly(conn)
					_, _ = io.Copy(conn, conn)
				}()
			}
		}()
	}
	must(t, json.NewEncoder(os.Stdout).Encode(Object{"tcp": tcp.Addr().String()}))
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestLinuxConnectionInventoryRealSockets(t *testing.T) {
	path := filepath.Join(testDir(t), "socket with spaces")
	exe, err := os.Executable()
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestLinuxConnectionInventoryProcess$")
	cmd.Env = append(os.Environ(), "CSC_TEST_CONNECTION_SOCKET="+path)
	stdin, err := cmd.StdinPipe()
	must(t, err)
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	cmd.Stderr = os.Stderr
	must(t, cmd.Start())
	defer func() {
		closeQuietly(stdin)
		if err := cmd.Wait(); err != nil {
			t.Errorf("connection fixture: %v", err)
		}
	}()
	var ready struct{ TCP string }
	must(t, json.NewDecoder(bufio.NewReader(stdout)).Decode(&ready))
	connect := func(network, address string) net.Conn {
		t.Helper()
		c, err := net.DialTimeout(network, address, time.Second)
		must(t, err)
		t.Cleanup(func() { closeQuietly(c) })
		must(t, c.SetDeadline(time.Now().Add(5*time.Second)))
		_, err = c.Write([]byte("ok"))
		must(t, err)
		body := make([]byte, 2)
		_, err = io.ReadFull(c, body)
		must(t, err)
		return c
	}
	connect("unix", path) // The already-initialized inspection connection.
	connect("unix", path+".other")
	connect("tcp4", ready.TCP)
	for _, want := range []int{0, 1, 3} {
		if want > 0 {
			connect("unix", path)
		}
		if want == 3 {
			connect("unix", path)
		}
		got, err := appServerConnections(ctx, cmd.Process.Pid, path)
		must(t, err)
		if got != want {
			t.Fatalf("got %d connections, want %d", got, want)
		}
	}
}

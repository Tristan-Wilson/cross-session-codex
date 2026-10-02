package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	connectionProcLimit = 8 << 20
	connectionFDLimit   = 65536
)

type connectionProcReader func(context.Context, string) ([]byte, error)

type connectionProcess struct {
	start, namespace string
	fds              map[string]string
}

type connectionSocket struct {
	protocol           string
	path               string
	flags, kind, state uint64
}

func appServerConnections(ctx context.Context, pid int, socket string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := linuxAppServerConnections(ctx, "/proc", pid, socket, readConnectionProc)
	if err != nil {
		return 0, fmt.Errorf("cannot inspect app-server connections: %w; nothing was stopped", err)
	}
	return n, nil
}

// Inspect only the verified server's descriptors: scanning client processes by
// UID would miss clients owned by another user. Socket inodes are distinct from
// filesystem socket inodes, and duplicated descriptors are not extra clients.
// Like lsof, procfs is not atomic. Stable repeated views reject observable churn;
// the caller still holds the launch lock and rechecks immediately before stopping.
func linuxAppServerConnections(ctx context.Context, root string, pid int, socket string, read connectionProcReader) (int, error) {
	if pid <= 0 || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return 0, errors.New("invalid connection inspection scope")
	}
	base := filepath.Join(root, strconv.Itoa(pid))
	before, err := snapshotConnectionProcess(ctx, base, pid, read)
	if err != nil {
		return 0, err
	}
	first, err := readConnectionSockets(ctx, base, before.fds, read)
	if err != nil {
		return 0, err
	}
	second, err := readConnectionSockets(ctx, base, before.fds, read)
	if err != nil {
		return 0, err
	}
	after, err := snapshotConnectionProcess(ctx, base, pid, read)
	if err != nil {
		return 0, err
	}
	if before.start != after.start || before.namespace != after.namespace ||
		!maps.Equal(before.fds, after.fds) || !maps.Equal(first, second) {
		return 0, errors.New("process, namespace, or socket inventory changed during inspection; retry")
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	listeners, connected := 0, 0
	for _, entry := range first {
		if entry.protocol != "unix" || entry.path != socket {
			continue
		}
		switch {
		case entry.kind == 1 && entry.flags == 0x10000 && entry.state == 1:
			listeners++
		case entry.kind == 1 && entry.flags == 0 && entry.state == 3:
			connected++
		default:
			return 0, errors.New("target socket has an unverified type or state")
		}
	}
	// Shutdown has already initialized and keeps its inspection connection open.
	// Require a separate connected inode; dup(listener) cannot stand in for it.
	if listeners != 1 || connected < 1 {
		return 0, errors.New("could not verify the listener and inspection connection")
	}
	return connected - 1, nil
}

func readConnectionProc(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(f)
	var result bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buffer)
		if result.Len()+n > connectionProcLimit {
			return nil, errors.New("connection proc file exceeds inspection limit")
		}
		result.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			return result.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func snapshotConnectionProcess(ctx context.Context, base string, pid int, read connectionProcReader) (connectionProcess, error) {
	var result connectionProcess
	stat, err := read(ctx, filepath.Join(base, "stat"))
	if err != nil {
		return result, err
	}
	text := string(stat)
	end := strings.LastIndex(text, ") ")
	if !strings.HasPrefix(text, strconv.Itoa(pid)+" (") || end < 0 {
		return result, errors.New("malformed process identity")
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 20 {
		return result, errors.New("truncated process identity")
	}
	if _, err = strconv.ParseUint(fields[19], 10, 64); err != nil {
		return result, errors.New("invalid process start identity")
	}
	result.start = fields[19]
	result.namespace, err = os.Readlink(filepath.Join(base, "ns", "net"))
	if err != nil {
		return result, err
	}
	if _, err = connectionLinkInode(result.namespace, "net"); err != nil {
		return result, err
	}
	dir, err := os.Open(filepath.Join(base, "fd"))
	if err != nil {
		return result, err
	}
	entries, err := dir.ReadDir(connectionFDLimit + 1)
	closeQuietly(dir)
	if err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}
	if len(entries) > connectionFDLimit {
		return result, errors.New("process descriptor count exceeds inspection limit")
	}
	result.fds = make(map[string]string, len(entries))
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if _, err = strconv.ParseUint(entry.Name(), 10, 32); err != nil {
			return result, errors.New("invalid process descriptor entry")
		}
		target, err := os.Readlink(filepath.Join(base, "fd", entry.Name()))
		if err != nil {
			return result, fmt.Errorf("cannot read process descriptor: %w", err)
		}
		result.fds[entry.Name()] = target
	}
	return result, nil
}

func connectionLinkInode(link, kind string) (uint64, error) {
	prefix := kind + ":["
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, "]") {
		return 0, errors.New("malformed kernel inode link")
	}
	n, err := strconv.ParseUint(link[len(prefix):len(link)-1], 10, 64)
	if err != nil || n == 0 {
		return 0, errors.New("invalid kernel inode link")
	}
	return n, nil
}

func readConnectionSockets(ctx context.Context, base string, fds map[string]string, read connectionProcReader) (map[uint64]connectionSocket, error) {
	wanted := map[uint64]bool{}
	for _, target := range fds {
		if strings.HasPrefix(target, "socket:") {
			inode, err := connectionLinkInode(target, "socket")
			if err != nil {
				return nil, err
			}
			wanted[inode] = true
		}
	}
	data, err := read(ctx, filepath.Join(base, "net", "unix"))
	if err != nil {
		return nil, err
	}
	unixSockets, err := parseConnectionUnix(data)
	if err != nil {
		return nil, err
	}
	result := map[uint64]connectionSocket{}
	for inode := range wanted {
		if entry, ok := unixSockets[inode]; ok {
			result[inode] = entry
		}
	}
	// An inode absent from the Unix table is not automatically non-Unix: the
	// table might be incomplete or the socket inherited from another namespace.
	// Positively classify ordinary Internet/netlink sockets, otherwise refuse.
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6", "netlink"} {
		if len(result) == len(wanted) {
			break
		}
		data, err = read(ctx, filepath.Join(base, "net", table))
		if errors.Is(err, os.ErrNotExist) {
			continue // An optional kernel protocol may be disabled.
		}
		if err != nil {
			return nil, err
		}
		inodes, err := parseConnectionOther(data, table)
		if err != nil {
			return nil, err
		}
		for inode := range inodes {
			if wanted[inode] {
				if _, exists := result[inode]; exists {
					return nil, errors.New("socket inode appears in multiple protocol tables")
				}
				result[inode] = connectionSocket{protocol: table}
			}
		}
	}
	if len(result) != len(wanted) {
		return nil, errors.New("cannot classify every server socket inode")
	}
	return result, nil
}

func connectionTableLines(data []byte) ([]string, error) {
	if len(data) == 0 || len(data) > connectionProcLimit || data[len(data)-1] != '\n' || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("empty, oversized, or truncated connection table")
	}
	return strings.Split(string(data[:len(data)-1]), "\n"), nil
}

func parseConnectionUnix(data []byte) (map[uint64]connectionSocket, error) {
	lines, err := connectionTableLines(data)
	if err != nil {
		return nil, err
	}
	if strings.Join(strings.Fields(lines[0]), " ") != "Num RefCount Protocol Flags Type St Inode Path" {
		return nil, errors.New("unrecognized Unix socket table header")
	}
	result := map[uint64]connectionSocket{}
	for _, line := range lines[1:] {
		var fields [7]string
		for i := range fields {
			line = strings.TrimLeft(line, " \t")
			end := strings.IndexAny(line, " \t")
			if end < 0 {
				end = len(line)
			}
			fields[i], line = line[:end], line[end:]
			if fields[i] == "" {
				return nil, errors.New("truncated Unix socket row")
			}
		}
		if !strings.HasSuffix(fields[0], ":") {
			return nil, errors.New("malformed Unix socket address")
		}
		fields[0] = strings.TrimSuffix(fields[0], ":")
		var values [7]uint64
		for i, field := range fields {
			base := 16
			if i == 6 {
				base = 10
			}
			values[i], err = strconv.ParseUint(field, base, 64)
			if err != nil {
				return nil, errors.New("invalid Unix socket field")
			}
		}
		if values[2] != 0 {
			return nil, errors.New("unrecognized Unix socket protocol")
		}
		if values[6] == 0 {
			continue // Kernel sockets without a userspace descriptor.
		}
		if _, exists := result[values[6]]; exists {
			return nil, errors.New("duplicate Unix socket inode")
		}
		result[values[6]] = connectionSocket{protocol: "unix", path: strings.TrimLeft(line, " \t"), flags: values[3], kind: values[4], state: values[5]}
	}
	return result, nil
}

func parseConnectionOther(data []byte, table string) (map[uint64]bool, error) {
	lines, err := connectionTableLines(data)
	if err != nil {
		return nil, err
	}
	header := strings.Join(strings.Fields(lines[0]), " ")
	expected := "sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode"
	if strings.HasSuffix(table, "6") {
		expected = strings.Replace(expected, "rem_address", "remote_address", 1)
	}
	if strings.HasPrefix(table, "udp") {
		expected += " ref pointer drops"
	}
	if table == "netlink" {
		expected = "sk Eth Pid Groups Rmem Wmem Dump Locks Drops Inode"
	}
	if header != expected {
		return nil, errors.New("unrecognized " + table + " socket table header")
	}
	result := map[uint64]bool{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		minimum := 12
		if table == "netlink" {
			minimum = 10
		} else if strings.HasPrefix(table, "udp") {
			minimum = 13
		}
		if len(fields) < minimum {
			return nil, errors.New("truncated " + table + " socket row")
		}
		if err := validateConnectionOtherFields(fields, table); err != nil {
			return nil, err
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			return nil, errors.New("invalid " + table + " socket inode")
		}
		if inode != 0 {
			if result[inode] {
				return nil, errors.New("duplicate " + table + " socket inode")
			}
			result[inode] = true
		}
	}
	return result, nil
}

func validateConnectionOtherFields(fields []string, table string) error {
	valid := func(value string, base int) bool {
		_, err := strconv.ParseUint(value, base, 64)
		return value != "" && err == nil
	}
	if table == "netlink" {
		for i, field := range fields {
			base := 10
			if i == 0 || i == 3 || i == 6 {
				base = 16
			}
			if !valid(field, base) {
				return errors.New("invalid netlink socket field")
			}
		}
		return nil
	}
	pair := func(value string, left, right int) bool {
		a, b, ok := strings.Cut(value, ":")
		if !ok || len(a) != left || len(b) != right || !valid(b, 16) {
			return false
		}
		// IPv6 addresses exceed uint64; validate their two halves separately.
		if left == 32 {
			return valid(a[:16], 16) && valid(a[16:], 16)
		}
		return valid(a, 16)
	}
	address := 8
	if strings.HasSuffix(table, "6") {
		address = 32
	}
	if !strings.HasSuffix(fields[0], ":") || !valid(strings.TrimSuffix(fields[0], ":"), 10) ||
		!pair(fields[1], address, 4) || !pair(fields[2], address, 4) || !valid(fields[3], 16) ||
		!pair(fields[4], 8, 8) || !pair(fields[5], 2, 8) || !valid(fields[6], 16) ||
		!valid(fields[7], 10) || !valid(fields[8], 10) || !valid(fields[10], 10) || !valid(fields[11], 16) {
		return errors.New("invalid " + table + " socket field")
	}
	for _, field := range fields[12:] {
		if _, err := strconv.ParseInt(field, 10, 64); err != nil {
			return errors.New("invalid " + table + " socket trailing field")
		}
	}
	return nil
}

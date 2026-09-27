package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestArchiveDeterministicContentsAndMetadata(t *testing.T) {
	t.Parallel()
	source, binary, inputs := archiveTestInputs(t)
	stamp := time.Date(2026, time.September, 27, 14, 20, 30, 0, time.FixedZone("source", 7200))
	first, second := filepath.Join(t.TempDir(), "first.tar.gz"), filepath.Join(t.TempDir(), "second.tar.gz")
	if err := makeArchive(first, binary, source, stamp); err != nil {
		t.Fatal(err)
	}
	// Host mtimes and permissions must not leak into the distributable.
	for name := range inputs {
		file := filepath.Join(source, filepath.FromSlash(name))
		if name == project {
			file = binary
		}
		if err := os.Chtimes(file, stamp.Add(time.Hour), stamp.Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := makeArchive(second, binary, source, stamp.UTC()); err != nil {
		t.Fatal(err)
	}
	firstBytes, secondBytes := archiveTestRead(t, first), archiveTestRead(t, second)
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("same contents and commit timestamp produced different archive bytes")
	}
	compressed, err := gzip.NewReader(bytes.NewReader(firstBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compressed.Close() }()
	if compressed.OS != 255 || !compressed.ModTime.IsZero() || compressed.Name != "" || compressed.Comment != "" {
		t.Fatalf("host-dependent gzip header: %+v", compressed.Header)
	}
	archive := tar.NewReader(compressed)
	var names []string
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		mode := int64(0o644)
		if header.Name == project {
			mode = 0o755
		}
		if header.Typeflag != tar.TypeReg || header.Mode != mode || !header.ModTime.Equal(stamp) ||
			header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
			!header.AccessTime.IsZero() || !header.ChangeTime.IsZero() || header.Format != tar.FormatUSTAR {
			t.Fatalf("noncanonical tar header: %+v", header)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != inputs[header.Name] || header.Size != int64(len(body)) {
			t.Fatalf("unexpected content for %q", header.Name)
		}
	}
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		t.Fatalf("gzip trailer: %v", err)
	}
	want := make([]string, 0, len(inputs))
	for name := range inputs {
		want = append(want, name)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("archive order/files = %v, want %v", names, want)
	}
}

func TestArchiveRefusesExistingOutputAndLinkedInputs(t *testing.T) {
	t.Parallel()
	t.Run("existing output", func(t *testing.T) {
		source, binary, _ := archiveTestInputs(t)
		output := filepath.Join(t.TempDir(), "existing")
		archiveTestWrite(t, output, []byte("operator data"), 0o644)
		if err := makeArchive(output, binary, source, time.Unix(123, 0)); err == nil {
			t.Fatal("existing output was accepted")
		}
		if string(archiveTestRead(t, output)) != "operator data" {
			t.Fatal("existing output changed")
		}
	})
	for _, name := range []string{project, "README.md", "LICENSE"} {
		t.Run(name, func(t *testing.T) {
			source, binary, _ := archiveTestInputs(t)
			input := filepath.Join(source, name)
			if name == project {
				input = binary
			}
			moved := input + ".real"
			if err := os.Rename(input, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, input); err != nil {
				t.Fatal(err)
			}
			if err := makeArchive(filepath.Join(t.TempDir(), "out.tar.gz"), binary, source, time.Unix(123, 0)); err == nil {
				t.Fatal("symlink input was accepted")
			}
		})
	}
}

func TestArchiveChecksumsCoverManifestAndEveryTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	manifest := Manifest{SchemaVersion: 1, Project: project, Version: "v1.2.3", Release: true}
	for _, target := range releaseTargets {
		target.Archive = project + "_v1.2.3_" + target.OS + "_" + target.Arch + ".tar.gz"
		body := []byte("archive for " + target.OS + "/" + target.Arch)
		archiveTestWrite(t, filepath.Join(dir, target.Archive), body, 0o644)
		digest, size, err := digestFile(filepath.Join(dir, target.Archive))
		if err != nil {
			t.Fatal(err)
		}
		if digest != archiveTestDigest(body) || size != int64(len(body)) {
			t.Fatal("digestFile returned inconsistent bytes or size")
		}
		target.ArchiveSHA256, target.Size = digest, size
		manifest.Targets = append(manifest.Targets, target)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	archiveTestWrite(t, filepath.Join(dir, "release.json"), body, 0o644)
	if err = writeChecksums(dir, manifest.Targets); err != nil {
		t.Fatal(err)
	}
	checksums := string(archiveTestRead(t, filepath.Join(dir, "SHA256SUMS")))
	if !strings.HasSuffix(checksums, "\n") {
		t.Fatal("checksum file lacks final newline")
	}
	lines := strings.Split(strings.TrimSuffix(checksums, "\n"), "\n")
	if len(lines) != len(manifest.Targets)+1 {
		t.Fatalf("checksum entries = %d, want %d", len(lines), len(manifest.Targets)+1)
	}
	var names []string
	for _, line := range lines {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || digest != archiveTestDigest(archiveTestRead(t, filepath.Join(dir, name))) {
			t.Fatalf("invalid checksum entry %q", line)
		}
		names = append(names, name)
	}
	if !sort.StringsAreSorted(names) || names[len(names)-1] != "release.json" {
		t.Fatalf("unexpected checksum ordering: %v", names)
	}
	// The caller's iteration order cannot affect the checksum bytes.
	for i, j := 0, len(manifest.Targets)-1; i < j; i, j = i+1, j-1 {
		manifest.Targets[i], manifest.Targets[j] = manifest.Targets[j], manifest.Targets[i]
	}
	if err = writeChecksums(dir, manifest.Targets); err != nil {
		t.Fatal(err)
	}
	if string(archiveTestRead(t, filepath.Join(dir, "SHA256SUMS"))) != checksums {
		t.Fatal("checksum bytes depend on target order")
	}
}

func TestArchiveSourceExtractionRejectsUnsafePathsAndTypes(t *testing.T) {
	t.Parallel()
	sandbox := t.TempDir()
	cases := []tar.Header{
		{Name: "../escaped", Typeflag: tar.TypeReg},
		{Name: filepath.Join(sandbox, "absolute-escaped"), Typeflag: tar.TypeReg},
		{Name: "./file", Typeflag: tar.TypeReg},
		{Name: "a/../escaped", Typeflag: tar.TypeReg},
		{Name: "a//file", Typeflag: tar.TypeReg},
		{Name: `a\file`, Typeflag: tar.TypeReg},
		{Name: ".", Typeflag: tar.TypeDir},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escaped"},
		{Name: "link", Typeflag: tar.TypeLink, Linkname: "../escaped"},
		{Name: "pipe", Typeflag: tar.TypeFifo},
		{Name: "device", Typeflag: tar.TypeChar},
	}
	for _, header := range cases {
		t.Run(header.Name+"/"+string(header.Typeflag), func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "source")
			if err := extractSource(bytes.NewReader(archiveTestTar(t, []tar.Header{header})), output); err == nil {
				t.Fatalf("accepted unsafe archive entry %+v", header)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(sandbox, "absolute-escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absolute traversal created an outside file: %v", err)
	}
}

func TestArchiveSourceExtractionNormalizesModesAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	headers := []tar.Header{
		{Name: "docs/", Typeflag: tar.TypeDir, Mode: 0o777},
		{Name: "docs/readme", Typeflag: tar.TypeReg, Mode: 0o666, Size: 3},
		{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o4777, Size: 3},
	}
	destination := filepath.Join(t.TempDir(), "source")
	if err := extractSource(bytes.NewReader(archiveTestTar(t, headers)), destination); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"docs": 0o755, "docs/readme": 0o644, "bin/tool": 0o755} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		// A restrictive caller umask may remove group/world access; extraction
		// must preserve owner access without granting archive-supplied bits.
		if info.Mode().Perm() & ^want != 0 || info.Mode().Perm()&0o700 != want&0o700 ||
			info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Fatalf("%s mode = %v, want at most %v without special bits", name, info.Mode(), want)
		}
	}
	if err := extractSource(bytes.NewReader(archiveTestTar(t, headers)), destination); err == nil {
		t.Fatal("accepted an existing extraction destination")
	}
	duplicate := []tar.Header{{Name: "duplicate", Typeflag: tar.TypeReg, Size: 3}, {Name: "duplicate", Typeflag: tar.TypeReg, Size: 3}}
	if err := extractSource(bytes.NewReader(archiveTestTar(t, duplicate)), filepath.Join(t.TempDir(), "duplicate")); err == nil {
		t.Fatal("accepted duplicate regular file entries")
	}
	truncated := archiveTestTar(t, []tar.Header{{Name: "truncated", Typeflag: tar.TypeReg, Size: 3}})
	if err := extractSource(bytes.NewReader(truncated[:513]), filepath.Join(t.TempDir(), "truncated")); err == nil {
		t.Fatal("accepted a truncated regular file payload")
	}
}

func TestArchiveExportUsesExactCommitAndPropagatesFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	destination := filepath.Join(t.TempDir(), "source")
	called := false
	command := func(_ context.Context, dir string, _ []string, name string, args ...string) ([]byte, error) {
		called = true
		want := []string{"archive", "--format=tar", "--output=" + filepath.Join(filepath.Dir(destination), "source.tar"), commit}
		if dir != root || name != "git" || !reflect.DeepEqual(args, want) {
			t.Fatalf("unexpected export command: dir=%q name=%q args=%q", dir, name, args)
		}
		archiveTestWrite(t, strings.TrimPrefix(args[2], "--output="), archiveTestTar(t, []tar.Header{{Name: "README.md", Typeflag: tar.TypeReg, Size: 3}}), 0o644)
		return nil, nil
	}
	if err := exportSource(t.Context(), root, destination, commit, command); err != nil {
		t.Fatal(err)
	}
	if !called || string(archiveTestRead(t, filepath.Join(destination, "README.md"))) != "xxx" {
		t.Fatal("export did not extract the exact archived source")
	}
	sentinel := errors.New("git archive failed")
	err := exportSource(t.Context(), root, filepath.Join(t.TempDir(), "failed"), commit,
		func(context.Context, string, []string, string, ...string) ([]byte, error) { return nil, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("git export error = %v, want sentinel", err)
	}
}

func archiveTestInputs(t *testing.T) (string, string, map[string]string) {
	t.Helper()
	source := t.TempDir()
	binary := filepath.Join(t.TempDir(), "binary")
	inputs := map[string]string{
		project: "\x7fELFfake binary\n", "README.md": "readme\n", "docs/LAUNCH_HANDSHAKE.md": "protocol\n",
		"LICENSE": "license\n", "LICENSE.md": "license markdown\n", "LICENSE.txt": "license text\n",
		"docs/RELEASING.md": "release instructions\n",
	}
	for name, body := range inputs {
		file := filepath.Join(source, filepath.FromSlash(name))
		if name == project {
			file = binary
		}
		archiveTestWrite(t, file, []byte(body), 0o600)
	}
	return source, binary, inputs
}

func archiveTestWrite(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func archiveTestRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func archiveTestDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func archiveTestTar(t *testing.T, headers []tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, header := range headers {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

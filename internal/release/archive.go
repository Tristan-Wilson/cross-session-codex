package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func exportSource(ctx context.Context, root, destination, commit string, command commandRunner) error {
	archive := filepath.Join(filepath.Dir(destination), "source.tar")
	if _, err := command(ctx, root, gitEnvironment(), "git", "archive", "--format=tar", "--output="+archive, commit); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return extractSource(f, destination)
}

func extractSource(reader io.Reader, destination string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue // Git's commit comment; no filesystem object is created.
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe source archive path %q", header.Name)
		}
		file := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(file, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, 0: // Also accept the legacy zero regular-file type.
			if err = os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, archive)
			if err = errors.Join(copyErr, out.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported source archive type %d for %q", header.Typeflag, header.Name)
		}
	}
}

func makeArchive(destination, binary, source string, stamp time.Time) error {
	inputs := map[string]string{
		project: binary, "README.md": filepath.Join(source, "README.md"),
		"docs/LAUNCH_HANDSHAKE.md": filepath.Join(source, "docs", "LAUNCH_HANDSHAKE.md"),
	}
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "docs/RELEASING.md"} {
		file := filepath.Join(source, filepath.FromSlash(name))
		if _, err := os.Lstat(file); err == nil {
			inputs[name] = file
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	compressed, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		_ = out.Close()
		return err
	}
	compressed.OS = 255
	archive := tar.NewWriter(compressed)
	for _, name := range names {
		info, err := os.Lstat(inputs[name])
		if err != nil || !info.Mode().IsRegular() {
			_ = archive.Close()
			_ = compressed.Close()
			_ = out.Close()
			return fmt.Errorf("archive input must be a regular file: %s (%v)", inputs[name], err)
		}
		mode := int64(0o644)
		if name == project {
			mode = 0o755
		}
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: info.Size(),
			ModTime: stamp.UTC(), Format: tar.FormatUSTAR}
		if err = archive.WriteHeader(header); err == nil {
			var input *os.File
			input, err = os.Open(inputs[name])
			if err == nil {
				_, copyErr := io.Copy(archive, input)
				err = errors.Join(copyErr, input.Close())
			}
		}
		if err != nil {
			_ = archive.Close()
			_ = compressed.Close()
			_ = out.Close()
			return err
		}
	}
	return errors.Join(archive.Close(), compressed.Close(), out.Close())
}

func digestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	digest := sha256.New()
	size, err := io.Copy(digest, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

func writeChecksums(directory string, targets []Target) error {
	checksums := map[string]string{}
	for _, target := range targets {
		checksums[target.Archive] = target.ArchiveSHA256
	}
	digest, _, err := digestFile(filepath.Join(directory, "release.json"))
	if err != nil {
		return err
	}
	checksums["release.json"] = digest
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var body strings.Builder
	for _, name := range names {
		_, _ = fmt.Fprintf(&body, "%s  %s\n", checksums[name], name)
	}
	return os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(body.String()), 0o644)
}

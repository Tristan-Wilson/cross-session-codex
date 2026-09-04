package bridge

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	assets "github.com/Tristan-Wilson/cross-session-codex"
)

const launcherMarker = "# Managed by cross-session-codex installer."
const skillMarker = "<!-- Managed by cross-session-codex installer. -->"

type InstallOptions struct {
	Prefix, BinDir, SkillDir string
	NoSkill                  bool
}

func defaultInstallOptions() InstallOptions {
	return InstallOptions{Prefix: filepath.Join(home(), ".local", "share", "cross-session-codex"), BinDir: filepath.Join(home(), ".local", "bin"), SkillDir: filepath.Join(envOr("CODEX_HOME", filepath.Join(home(), ".codex")), "skills", "cross-session-messaging")}
}
func ownedDirectory(path string) error {
	ancestor := path
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		ancestor = filepath.Dir(ancestor)
	}
	if err := vetParents(ancestor); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	if err := vetParents(path); err != nil {
		return err
	}
	_, err := owned(path, os.ModeDir, false)
	return err
}
func ensureManaged(path, marker string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := owned(path, 0, false); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Contains(b, []byte(marker)) {
		return fmt.Errorf("refusing to overwrite unrelated file: %s", path)
	}
	return nil
}
func replaceFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cross-session-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer closeQuietly(f)
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
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
func Install(opts InstallOptions) (Object, error) {
	if _, err := exec.LookPath("codex"); err != nil {
		return nil, errors.New("codex CLI must be installed and on PATH")
	}
	var err error
	opts.Prefix, err = filepath.Abs(opts.Prefix)
	if err != nil {
		return nil, err
	}
	opts.BinDir, err = filepath.Abs(opts.BinDir)
	if err != nil {
		return nil, err
	}
	opts.SkillDir, err = filepath.Abs(opts.SkillDir)
	if err != nil {
		return nil, err
	}
	launcher := filepath.Join(opts.BinDir, "cross-session-codex")
	if err = ensureManaged(launcher, launcherMarker); err != nil {
		return nil, err
	}
	if !opts.NoSkill {
		if err = ensureManaged(filepath.Join(opts.SkillDir, "SKILL.md"), skillMarker); err != nil {
			return nil, err
		}
	}
	current := filepath.Join(opts.Prefix, "current")
	if st, e := os.Lstat(current); e == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("refusing to replace unrelated install path: %s", current)
		}
		target, e := filepath.EvalSymlinks(current)
		if e != nil {
			return nil, e
		}
		rel, e := filepath.Rel(filepath.Join(opts.Prefix, "releases"), target)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, errors.New("current install link points outside managed releases")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	for _, dir := range []string{opts.Prefix, filepath.Join(opts.Prefix, "releases"), opts.BinDir} {
		if err = ownedDirectory(dir); err != nil {
			return nil, err
		}
	}
	lockPath := filepath.Join(opts.Prefix, "install.lock")
	if _, e := os.Lstat(lockPath); e == nil {
		if _, err = owned(lockPath, 0, true); err != nil {
			return nil, err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, err
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"cross-session-codex": binary, "README.md": []byte(assets.Readme), "SKILL.md": []byte(assets.MessagingSkill)}
	digest := sha256.New()
	for _, name := range []string{"cross-session-codex", "README.md", "SKILL.md"} {
		_, _ = digest.Write([]byte(name + "\x00"))
		_, _ = digest.Write(files[name])
		_, _ = digest.Write([]byte{0})
	}
	id := fmt.Sprintf("%x", digest.Sum(nil))[:20]
	releases := filepath.Join(opts.Prefix, "releases")
	release := filepath.Join(releases, id)
	if _, err = os.Lstat(release); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(releases, ".install-")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(stage) }()
		for name, data := range files {
			mode := os.FileMode(0644)
			if name == "cross-session-codex" {
				mode = 0755
			}
			if err = replaceFile(filepath.Join(stage, name), data, mode); err != nil {
				return nil, err
			}
		}
		if err = os.Rename(stage, release); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if _, err = owned(release, os.ModeDir, false); err != nil {
			return nil, err
		}
		for name, data := range files {
			p := filepath.Join(release, name)
			if _, err = owned(p, 0, false); err != nil {
				return nil, err
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return nil, e
			}
			if !bytes.Equal(b, data) {
				return nil, fmt.Errorf("installed release was modified: %s", p)
			}
		}
	}
	link := filepath.Join(opts.Prefix, ".current-"+id)
	if err = os.Symlink(filepath.Join("releases", id), link); err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(link) }()
	if err = os.Rename(link, current); err != nil {
		return nil, err
	}
	text := "#!/bin/sh\n" + launcherMarker + "\nexec " + shellQuote(filepath.Join(current, "cross-session-codex")) + " \"$@\"\n"
	if err = replaceFile(launcher, []byte(text), 0755); err != nil {
		return nil, err
	}
	if !opts.NoSkill {
		if err = ownedDirectory(opts.SkillDir); err != nil {
			return nil, err
		}
		skill := strings.Replace(assets.MessagingSkill, "name: messaging\n", "name: cross-session-messaging\n", 1) + "\n" + skillMarker + "\n\nThe installed CLI is `" + shellQuote(launcher) + "`. Use this absolute path from the user's project if the command is not on PATH.\n"
		if err = replaceFile(filepath.Join(opts.SkillDir, "SKILL.md"), []byte(skill), 0644); err != nil {
			return nil, err
		}
	}
	return Object{"command": launcher, "release": release, "skill": opts.SkillDir, "version": Version, "launch": shellQuote(launcher) + " launch", "note": "Existing workers keep their code until restarted. Launch Codex from your project directory; no Python, tmux or source checkout is needed at runtime."}, nil
}

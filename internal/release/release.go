// Package release builds reproducible local artifacts without installing or
// publishing them. Release metadata comes from Git, never caller-supplied
// version, commit, date, or compiler flags.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const project = "cross-session-codex"
const module = "github.com/Tristan-Wilson/cross-session-codex"
const toolchain = "go1.27.1"

type Options struct {
	Source, Output, Tag string
	Snapshot            bool
	Build               bool
	CrossBuild          bool
}

type Target struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Archive       string `json:"archive,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	BinarySHA256  string `json:"binary_sha256"`
	Size          int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	Project       string   `json:"project"`
	Version       string   `json:"version"`
	Tag           string   `json:"tag,omitempty"`
	SourceCommit  string   `json:"source_commit"`
	SourceDirty   bool     `json:"source_dirty"`
	Release       bool     `json:"release"`
	BuildDate     string   `json:"build_date"`
	GoVersion     string   `json:"go_version"`
	Targets       []Target `json:"targets"`
}

type sourceState struct {
	commit, fingerprint string
	date                time.Time
	dirty               bool
	tags                []string
}

var releaseTargets = []Target{
	{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
	{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
}

// Run creates artifacts locally. Package output must not exist; native/cross
// modes may replace their exact build files, but never tracked source files.
func Run(ctx context.Context, opts Options) (Manifest, error) {
	return run(ctx, opts, runCommand)
}

func run(ctx context.Context, opts Options, command commandRunner) (Manifest, error) {
	var empty Manifest
	modeCount := 0
	for _, selected := range []bool{opts.Tag != "", opts.Snapshot, opts.Build, opts.CrossBuild} {
		if selected {
			modeCount++
		}
	}
	if modeCount != 1 || opts.Output == "" {
		return empty, errors.New("choose exactly one of --tag TAG, --snapshot, --build, --cross-build and supply --out")
	}
	if opts.Tag != "" && !validTag(opts.Tag) {
		return empty, errors.New("release tag must be a complete v-prefixed semantic version")
	}
	root, err := repositoryRoot(ctx, opts.Source, command)
	if err != nil {
		return empty, err
	}
	before, err := inspectSource(ctx, root, "", command)
	if err != nil {
		return empty, err
	}
	manifest, err := metadata(before, opts)
	if err != nil {
		return empty, err
	}
	if err = checkToolchain(ctx, root, command); err != nil {
		return empty, err
	}
	output, err := filepath.Abs(opts.Output)
	if err != nil {
		return empty, err
	}
	output = filepath.Clean(output)
	if opts.CrossBuild {
		if info, err := os.Lstat(output); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return empty, errors.New("cross-build output directory must not be a symlink")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return empty, err
		}
	}
	output, err = safeOutput(ctx, root, output, command)
	if err != nil {
		return empty, err
	}
	packaging := !opts.Build && !opts.CrossBuild
	if packaging {
		if _, err = os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			return empty, fmt.Errorf("package output must not exist: %s", output)
		}
	}
	parent := filepath.Dir(output)
	if opts.CrossBuild {
		parent = output
	}
	if err = os.MkdirAll(parent, 0o755); err != nil {
		return empty, err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return empty, fmt.Errorf("resolve output parent: %w", err)
	}
	if opts.CrossBuild {
		output = parent
	} else {
		output = filepath.Join(parent, filepath.Base(output))
	}
	stage, err := os.MkdirTemp(parent, ".csc-release-")
	if err != nil {
		return empty, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	work := filepath.Join(stage, "work")
	artifacts := filepath.Join(stage, "artifacts")
	if err = os.Mkdir(work, 0o700); err != nil {
		return empty, err
	}
	if err = os.Mkdir(artifacts, 0o755); err != nil {
		return empty, err
	}
	buildRoot := root
	if manifest.Release {
		buildRoot = filepath.Join(work, "source")
		if err = exportSource(ctx, root, buildRoot, before.commit, command); err != nil {
			return empty, err
		}
	}
	targets := append([]Target(nil), releaseTargets...)
	if opts.Build {
		targets = []Target{{OS: runtime.GOOS, Arch: runtime.GOARCH}}
	}
	if !packaging {
		for _, target := range targets {
			destination := output
			if opts.CrossBuild {
				destination = filepath.Join(output, rawName(target))
			}
			if err = checkBuildDestination(ctx, root, destination, command); err != nil {
				return empty, err
			}
		}
	}
	for _, target := range targets {
		binary := filepath.Join(work, rawName(target))
		if err = compile(ctx, buildRoot, binary, target, manifest, command); err != nil {
			return empty, err
		}
		target.BinarySHA256, target.Size, err = digestFile(binary)
		if err != nil {
			return empty, err
		}
		if packaging {
			target.Archive = fmt.Sprintf("%s_%s_%s_%s.tar.gz", project, manifest.Version, target.OS, target.Arch)
			archive := filepath.Join(artifacts, target.Archive)
			if err = makeArchive(archive, binary, buildRoot, before.date); err != nil {
				return empty, err
			}
			target.ArchiveSHA256, target.Size, err = digestFile(archive)
			if err != nil {
				return empty, err
			}
		}
		manifest.Targets = append(manifest.Targets, target)
	}
	if packaging {
		body, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return empty, err
		}
		if err = os.WriteFile(filepath.Join(artifacts, "release.json"), append(body, '\n'), 0o644); err != nil {
			return empty, err
		}
		if err = writeChecksums(artifacts, manifest.Targets); err != nil {
			return empty, err
		}
	}
	after, err := inspectSource(ctx, root, stage, command)
	if err != nil {
		return empty, err
	}
	if before.fingerprint != after.fingerprint {
		return empty, errors.New("source state changed while building; no artifacts published")
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	if packaging {
		if err = publishDirectory(artifacts, output); err != nil {
			return empty, fmt.Errorf("publish new output directory: %w", err)
		}
	} else {
		// Compile every target before replacing any conventional build artifact.
		// Other files in an existing cross-build directory are left untouched.
		for _, target := range targets {
			destination := output
			if opts.CrossBuild {
				destination = filepath.Join(output, rawName(target))
			}
			if err = checkBuildDestination(ctx, root, destination, command); err != nil {
				return empty, err
			}
			if err = os.Rename(filepath.Join(work, rawName(target)), destination); err != nil {
				return empty, fmt.Errorf("publish build artifact: %w", err)
			}
		}
	}
	return manifest, nil
}

func rawName(target Target) string { return project + "-" + target.OS + "-" + target.Arch }

// Resolve existing parent aliases before creating any directories. In
// particular, raw build outputs must never overwrite repository metadata,
// including a linked worktree's external Git directory or shared common dir.
func safeOutput(ctx context.Context, root, output string, command commandRunner) (string, error) {
	parent := filepath.Dir(output)
	var missing []string
	for {
		if _, err := os.Lstat(parent); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		parent = filepath.Join(parent, missing[i])
	}
	output = filepath.Join(parent, filepath.Base(output))
	protected := []string{filepath.Join(root, ".git")}
	for _, option := range []string{"--absolute-git-dir", "--git-common-dir"} {
		body, err := command(ctx, root, gitEnvironment(), "git", "rev-parse", option)
		if err != nil {
			return "", err
		}
		dir := strings.TrimSpace(string(body))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		dir, err = filepath.EvalSymlinks(dir)
		if err != nil {
			return "", err
		}
		protected = append(protected, dir)
	}
	for _, dir := range protected {
		rel, err := filepath.Rel(dir, output)
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("artifact output must not be inside Git metadata")
		}
	}
	return output, nil
}

func repositoryRoot(ctx context.Context, source string, command commandRunner) (string, error) {
	if source == "" {
		source = "."
	}
	body, err := command(ctx, source, gitEnvironment(), "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve source repository: %w", err)
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(body)))
	if err != nil {
		return "", err
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module "+module+"\n") {
		return "", errors.New("source must be the cross-session-codex module")
	}
	return root, nil
}

func inspectSource(ctx context.Context, root, stage string, command commandRunner) (sourceState, error) {
	var source sourceState
	git := func(args ...string) ([]byte, error) { return command(ctx, root, gitEnvironment(), "git", args...) }
	head, err := git("show", "-s", "--format=%H%n%ct", "HEAD")
	if err != nil {
		return source, err
	}
	fields := strings.Fields(string(head))
	if len(fields) != 2 || (len(fields[0]) != 40 && len(fields[0]) != 64) {
		return source, errors.New("git returned invalid source metadata")
	}
	if _, err = hex.DecodeString(fields[0]); err != nil {
		return source, errors.New("git returned invalid commit ID")
	}
	seconds, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || seconds < 0 {
		return source, errors.New("git returned invalid commit timestamp")
	}
	source.commit, source.date = fields[0], time.Unix(seconds, 0).UTC()
	paths := []string{"--", "."}
	if stage != "" {
		if rel, err := filepath.Rel(root, stage); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			paths = append(paths, ":(exclude,literal)"+filepath.ToSlash(rel))
		}
	}
	status, err := git(append([]string{"status", "--porcelain=v1", "--untracked-files=all"}, paths...)...)
	if err != nil {
		return source, err
	}
	diff, err := git(append([]string{"diff", "--binary", "--no-ext-diff", "HEAD"}, paths...)...)
	if err != nil {
		return source, err
	}
	tags, err := git("tag", "--points-at", "HEAD")
	if err != nil {
		return source, err
	}
	for _, tag := range strings.Fields(string(tags)) {
		if validTag(tag) {
			source.tags = append(source.tags, tag)
		}
	}
	sort.Strings(source.tags)
	source.dirty = len(status) != 0
	fingerprint := sha256.New()
	for _, part := range [][]byte{head, status, diff, tags} {
		_, _ = fingerprint.Write(part)
		_, _ = fingerprint.Write([]byte{0})
	}
	source.fingerprint = hex.EncodeToString(fingerprint.Sum(nil))
	return source, nil
}

func metadata(source sourceState, opts Options) (Manifest, error) {
	m := Manifest{SchemaVersion: 1, Project: project, SourceCommit: source.commit,
		SourceDirty: source.dirty, BuildDate: source.date.Format(time.RFC3339), GoVersion: toolchain}
	if opts.Tag != "" {
		if source.dirty {
			return m, errors.New("release requires a clean tracked and untracked source tree")
		}
		found := false
		for _, tag := range source.tags {
			found = found || tag == opts.Tag
		}
		if !found {
			return m, fmt.Errorf("release tag %s does not name HEAD", opts.Tag)
		}
		m.Version, m.Tag, m.Release = opts.Tag, opts.Tag, true
		return m, nil
	}
	if !opts.Snapshot && !source.dirty && len(source.tags) > 0 {
		if len(source.tags) != 1 {
			return m, errors.New("multiple version tags name HEAD; source build version is ambiguous")
		}
		m.Version, m.Tag = source.tags[0], source.tags[0]
		return m, nil
	}
	m.Version = "v0.0.0-snapshot." + source.commit[:12]
	if source.dirty {
		m.Version += ".dirty"
	}
	return m, nil
}

func validTag(tag string) bool {
	if !strings.HasPrefix(tag, "v") {
		return false
	}
	version := strings.TrimPrefix(tag, "v")
	coreAndPre, build, hasBuild := strings.Cut(version, "+")
	if hasBuild && !validIdentifiers(build, false) {
		return false
	}
	core, pre, hasPre := strings.Cut(coreAndPre, "-")
	if hasPre && !validIdentifiers(pre, true) {
		return false
	}
	numbers := strings.Split(core, ".")
	if len(numbers) != 3 {
		return false
	}
	for _, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return false
		}
		for _, digit := range number {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

func validIdentifiers(value string, noNumericLeadingZero bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, char := range identifier {
			if char < '0' || char > '9' {
				numeric = false
				if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && char != '-' {
					return false
				}
			}
		}
		if noNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func checkBuildDestination(ctx context.Context, root, path string, command commandRunner) error {
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("build output must be a regular file, not a symlink or directory: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		body, err := command(ctx, root, gitEnvironment(), "git", "ls-files", "--", ":(literal)"+filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if len(body) != 0 {
			return fmt.Errorf("refusing to replace tracked source: %s", path)
		}
	}
	return nil
}

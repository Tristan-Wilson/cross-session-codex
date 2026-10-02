package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type testCompiler struct {
	builds      int
	failAt      int
	wrongGo     bool
	roots       []string
	envs        []map[string]string
	args        [][]string
	beforeBuild func(int) error
}

func (f *testCompiler) command(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name != "go" {
		return runCommand(ctx, dir, env, name, args...)
	}
	if reflect.DeepEqual(args, []string{"env", "GOVERSION"}) {
		if f.wrongGo {
			return []byte("go1.26.0\n"), nil
		}
		return []byte(toolchain + "\n"), nil
	}
	if len(args) < 2 || args[0] != "build" {
		return nil, fmt.Errorf("unexpected fake Go command: %q", args)
	}
	f.builds++
	f.roots = append(f.roots, dir)
	f.args = append(f.args, append([]string(nil), args...))
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	f.envs = append(f.envs, values)
	if f.beforeBuild != nil {
		if err := f.beforeBuild(f.builds); err != nil {
			return nil, err
		}
	}
	var output, linker string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-o" {
			output = args[i+1]
		}
		if args[i] == "-ldflags" {
			linker = args[i+1]
		}
	}
	if output == "" || linker == "" {
		return nil, errors.New("missing fixed compiler arguments")
	}
	if err := os.WriteFile(output, []byte(values["GOOS"]+"/"+values["GOARCH"]+"\n"+linker+"\n"), 0o755); err != nil {
		return nil, err
	}
	if f.failAt == f.builds {
		return nil, errors.New("simulated build failure after partial binary")
	}
	return nil, nil
}

func releaseTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":     "module " + module + "\n\ngo 1.27.0\n\ntoolchain go1.27.1\n",
		".gitignore": "dist/\n", "README.md": "release fixture\n",
		"docs/LAUNCH_HANDSHAKE.md": "handshake fixture\n", "docs/RELEASING.md": "release guide\n",
		"cmd/cross-session-codex/main.go": "package main\nfunc main() {}\n",
	} {
		file := filepath.Join(dir, filepath.FromSlash(name))
		releaseMust(t, os.MkdirAll(filepath.Dir(file), 0o755))
		releaseMust(t, os.WriteFile(file, []byte(body), 0o644))
	}
	releaseGit(t, dir, "init", "--quiet")
	releaseGit(t, dir, "add", ".")
	releaseGit(t, dir, "-c", "user.name=Release Test", "-c", "user.email=release-test@example.invalid", "commit", "--quiet", "-m", "fixture")
	return dir
}

func releaseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	env := append(gitEnvironment(), "GIT_AUTHOR_DATE=2023-11-14T22:13:20Z", "GIT_COMMITTER_DATE=2023-11-14T22:13:20Z")
	body, err := runCommand(context.Background(), dir, env, "git", args...)
	releaseMust(t, err)
	return strings.TrimSpace(string(body))
}

func releaseMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func releaseRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	releaseMust(t, err)
	return body
}

func TestReleasePackageCleanAnnotatedTagAndReproducibility(t *testing.T) {
	root := releaseTestRepo(t)
	releaseGit(t, root, "-c", "user.name=Release Test", "-c", "user.email=release-test@example.invalid", "tag", "-a", "v0.1.3", "-m", "release")
	var first Manifest
	var firstOutput string
	for i := range 2 {
		compiler := &testCompiler{}
		output := filepath.Join(root, "dist", fmt.Sprintf("release-%d", i))
		manifest, err := run(context.Background(), Options{Source: root, Output: output, Tag: "v0.1.3"}, compiler.command)
		releaseMust(t, err)
		if !manifest.Release || manifest.SourceDirty || manifest.Version != "v0.1.3" || manifest.Tag != manifest.Version || manifest.SchemaVersion != 1 || manifest.Project != project || manifest.GoVersion != toolchain || manifest.BuildDate != "2023-11-14T22:13:20Z" {
			t.Fatalf("wrong release provenance: %+v", manifest)
		}
		if manifest.SourceCommit != releaseGit(t, root, "rev-parse", "HEAD") || len(manifest.Targets) != 4 || compiler.builds != 4 {
			t.Fatalf("wrong source or targets: %+v builds=%d", manifest, compiler.builds)
		}
		for _, buildRoot := range compiler.roots {
			if buildRoot == root || !strings.HasSuffix(buildRoot, filepath.Join("work", "source")) {
				t.Fatalf("release compiled mutable worktree: %q", buildRoot)
			}
		}
		entries, err := os.ReadDir(output)
		releaseMust(t, err)
		if len(entries) != 6 {
			t.Fatalf("package should contain four archives, manifest, checksums: %v", entries)
		}
		if i == 0 {
			first, firstOutput = manifest, output
		} else {
			if !reflect.DeepEqual(first, manifest) {
				t.Fatalf("same source produced different metadata: %+v vs %+v", first, manifest)
			}
			for _, entry := range entries {
				if string(releaseRead(t, filepath.Join(output, entry.Name()))) != string(releaseRead(t, filepath.Join(firstOutput, entry.Name()))) {
					t.Fatalf("artifact is not reproducible: %s", entry.Name())
				}
			}
		}
	}
}

func TestReleaseRejectsBadSourceBeforeBuilding(t *testing.T) {
	for _, scenario := range []string{"missing-tag", "mismatched-tag", "dirty-tracked", "dirty-untracked", "invalid-tag"} {
		t.Run(scenario, func(t *testing.T) {
			root := releaseTestRepo(t)
			tag := "v0.1.3"
			if scenario != "missing-tag" {
				releaseGit(t, root, "tag", tag)
			}
			switch scenario {
			case "mismatched-tag":
				releaseGit(t, root, "-c", "user.name=Release Test", "-c", "user.email=release-test@example.invalid", "commit", "--allow-empty", "--quiet", "-m", "new HEAD")
			case "dirty-tracked":
				releaseMust(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("modified"), 0o644))
			case "dirty-untracked":
				releaseMust(t, os.WriteFile(filepath.Join(root, "new.go"), []byte("package main"), 0o644))
			case "invalid-tag":
				tag = "v0.1.3 -X forged=1"
			}
			output := filepath.Join(root, "dist", "release")
			compiler := &testCompiler{}
			if _, err := run(context.Background(), Options{Source: root, Output: output, Tag: tag}, compiler.command); err == nil || compiler.builds != 0 {
				t.Fatalf("unsafe source accepted: error=%v builds=%d", err, compiler.builds)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected release published output: %v", err)
			}
		})
	}
}

func TestReleaseFailureNeverPublishesOrOverwrites(t *testing.T) {
	for _, scenario := range []string{"compile-failure", "source-changed", "tag-moved", "destination-race", "wrong-toolchain"} {
		t.Run(scenario, func(t *testing.T) {
			root := releaseTestRepo(t)
			releaseGit(t, root, "tag", "v0.1.3")
			output := filepath.Join(t.TempDir(), "release")
			compiler := &testCompiler{}
			compiler.beforeBuild = func(index int) error {
				if index != 2 {
					return nil
				}
				switch scenario {
				case "source-changed":
					return os.WriteFile(filepath.Join(root, "README.md"), []byte("changed during build"), 0o644)
				case "tag-moved":
					releaseGit(t, root, "tag", "-d", "v0.1.3")
				case "destination-race":
					if err := os.Mkdir(output, 0o755); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(output, "owner"), []byte("someone else's directory"), 0o600)
				}
				return nil
			}
			if scenario == "compile-failure" {
				compiler.failAt = 2
			}
			compiler.wrongGo = scenario == "wrong-toolchain"
			if _, err := run(context.Background(), Options{Source: root, Output: output, Tag: "v0.1.3"}, compiler.command); err == nil {
				t.Fatal("failed/racing release unexpectedly published")
			}
			if scenario == "destination-race" {
				if got := string(releaseRead(t, filepath.Join(output, "owner"))); got != "someone else's directory" {
					t.Fatal("publication overwrote the concurrent output owner")
				}
			} else if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed build left partial public output: %v", err)
			}
			staging, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".csc-release-*"))
			releaseMust(t, err)
			if len(staging) != 0 {
				t.Fatalf("failed build left staging artifacts: %v", staging)
			}
		})
	}
}

func TestReleaseSnapshotAndSourceBuildModes(t *testing.T) {
	// Exercise a source alias on every host, not only macOS where temporary
	// paths commonly spell /private/var as /var. The compiler must use the
	// selected directory, but its canonical path need not retain that spelling.
	repository := releaseTestRepo(t)
	root := filepath.Join(t.TempDir(), "source-alias")
	releaseMust(t, os.Symlink(repository, root))
	releaseGit(t, root, "tag", "v0.1.3")
	compiler := &testCompiler{}
	manifest, err := run(context.Background(), Options{Source: root, Output: filepath.Join(root, "dist", "snapshot"), Snapshot: true}, compiler.command)
	releaseMust(t, err)
	if manifest.Release || manifest.SourceDirty || manifest.Tag != "" || !strings.HasPrefix(manifest.Version, "v0.0.0-snapshot.") {
		t.Fatalf("tagged snapshot mislabeled as release: %+v", manifest)
	}
	selectedDirectory, err := os.Stat(root)
	releaseMust(t, err)
	for _, source := range compiler.roots {
		compiledDirectory, err := os.Stat(source)
		releaseMust(t, err)
		if !os.SameFile(compiledDirectory, selectedDirectory) {
			t.Fatalf("snapshot did not compile selected worktree: %q", source)
		}
	}
	binary := filepath.Join(root, "dist", project)
	compiler = &testCompiler{}
	manifest, err = run(context.Background(), Options{Source: root, Output: binary, Build: true}, compiler.command)
	releaseMust(t, err)
	if manifest.Release || manifest.Version != "v0.1.3" || compiler.builds != 1 || compiler.envs[0]["GOOS"] != runtime.GOOS || compiler.envs[0]["GOARCH"] != runtime.GOARCH {
		t.Fatalf("wrong native build metadata/target: %+v %v", manifest, compiler.envs)
	}
	releaseMust(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("local edits"), 0o644))
	compiler = &testCompiler{}
	manifest, err = run(context.Background(), Options{Source: root, Output: binary, Build: true}, compiler.command)
	releaseMust(t, err)
	if !manifest.SourceDirty || !strings.HasSuffix(manifest.Version, ".dirty") || manifest.Tag != "" {
		t.Fatalf("dirty source build hid local edits: %+v", manifest)
	}
	oldBinary := string(releaseRead(t, binary))
	if _, err = run(context.Background(), Options{Source: root, Output: binary, Build: true}, (&testCompiler{failAt: 1}).command); err == nil {
		t.Fatal("failed native build succeeded")
	}
	if string(releaseRead(t, binary)) != oldBinary {
		t.Fatal("failed native build replaced the previous artifact")
	}
	cross := filepath.Join(root, "dist", "cross")
	releaseMust(t, os.Mkdir(cross, 0o755))
	releaseMust(t, os.WriteFile(filepath.Join(cross, "unrelated"), []byte("keep"), 0o600))
	manifest, err = run(context.Background(), Options{Source: root, Output: cross, CrossBuild: true}, (&testCompiler{}).command)
	releaseMust(t, err)
	for _, target := range manifest.Targets {
		if _, err := os.Stat(filepath.Join(cross, rawName(target))); err != nil {
			t.Fatal(err)
		}
	}
	if string(releaseRead(t, filepath.Join(cross, "unrelated"))) != "keep" {
		t.Fatal("cross build overwrote unrelated output")
	}
}

func TestReleaseFixedCompilerAndGitEnvironment(t *testing.T) {
	root := releaseTestRepo(t)
	for key, value := range map[string]string{
		"GOFLAGS": "-ldflags=-X forged=1", "GOOS": "windows", "GOARCH": "386",
		"GOAMD64": "v4", "GOARM64": "v9.5", "GOEXPERIMENT": "anything",
		"GOTOOLCHAIN": "local", "GOENV": "/malicious", "GOWORK": "/other/go.work",
		"CGO_ENABLED": "1", "CGO_CFLAGS": "-anything", "GOROOT": "/different/toolchain",
		"GIT_DIR": "/different/git", "GIT_WORK_TREE": "/different/tree", "GIT_CONFIG_COUNT": "1",
	} {
		t.Setenv(key, value)
	}
	compiler := &testCompiler{}
	_, err := run(context.Background(), Options{Source: root, Output: filepath.Join(root, "dist", "snapshot"), Snapshot: true}, compiler.command)
	releaseMust(t, err)
	for i, env := range compiler.envs {
		if env["GOOS"] != releaseTargets[i].OS || env["GOARCH"] != releaseTargets[i].Arch || env["CGO_ENABLED"] != "0" || env["GOTOOLCHAIN"] != toolchain || env["GOENV"] != "off" || env["GOWORK"] != "off" || env["GOFLAGS"] != "" {
			t.Fatalf("ambient environment changed compiler settings: %v", env)
		}
		if env["GOEXPERIMENT"] != "" || env["CGO_CFLAGS"] != "" || env["GOROOT"] != "" {
			t.Fatalf("unsafe compiler override survived: %v", env)
		}
		args := strings.Join(compiler.args[i], " ")
		for _, required := range []string{"-mod=readonly", "-trimpath", "-buildvcs=false", "-buildid=", module + "/internal/bridge.BuildCommit="} {
			if !strings.Contains(args, required) {
				t.Fatalf("fixed compiler option missing: %s from %v", required, compiler.args[i])
			}
		}
		if strings.Contains(args, "forged") {
			t.Fatal("caller flags forged build metadata")
		}
	}
}

func TestReleaseOutputAndTagBoundaries(t *testing.T) {
	root := releaseTestRepo(t)
	output := filepath.Join(t.TempDir(), "existing")
	releaseMust(t, os.Mkdir(output, 0o755))
	for _, opts := range []Options{
		{Source: root, Output: output, Snapshot: true},
		{Source: root, Output: filepath.Join(root, "README.md"), Build: true},
		{Source: root, Output: filepath.Join(root, "dist", "ambiguous"), Build: true, Snapshot: true},
	} {
		compiler := &testCompiler{}
		if _, err := run(context.Background(), opts, compiler.command); err == nil || compiler.builds != 0 {
			t.Fatalf("unsafe output/options accepted: %v error=%v builds=%d", opts, err, compiler.builds)
		}
	}
	releaseGit(t, root, "tag", "v0.1.3")
	releaseGit(t, root, "tag", "v0.2.0")
	compiler := &testCompiler{}
	if _, err := run(context.Background(), Options{Source: root, Output: filepath.Join(root, "dist", "native"), Build: true}, compiler.command); err == nil || compiler.builds != 0 {
		t.Fatalf("ambiguous native tag was silently selected: %v", err)
	}
	if _, err := run(context.Background(), Options{Source: root, Output: filepath.Join(root, "dist", "explicit"), Tag: "v0.1.3"}, compiler.command); err != nil {
		t.Fatalf("explicit release tag could not disambiguate: %v", err)
	}
}

func TestValidReleaseTags(t *testing.T) {
	for _, tag := range []string{"v0.0.0", "v1.2.3", "v1.2.3-rc.1", "v1.2.3+build.01", "v1.2.3-alpha-beta+build.2"} {
		if !validTag(tag) {
			t.Errorf("valid tag rejected: %s", tag)
		}
	}
	for _, tag := range []string{"1.2.3", "v1.2", "v01.2.3", "v1.2.3-01", "v1.2.3-", "v1.2.3+", "v1.2.3+a+b", "v1.2.3\n", "v1.2.3 -X x=y", "v1.2.3/other"} {
		if validTag(tag) {
			t.Errorf("unsafe/invalid tag accepted: %q", tag)
		}
	}
}

func TestReleaseOutputCannotOverwriteGitMetadata(t *testing.T) {
	root := releaseTestRepo(t)
	gitConfig := filepath.Join(root, ".git", "config")
	before := string(releaseRead(t, gitConfig))
	alias := filepath.Join(t.TempDir(), "git-alias")
	releaseMust(t, os.Symlink(filepath.Join(root, ".git"), alias))
	worktree := filepath.Join(t.TempDir(), "linked")
	releaseGit(t, root, "worktree", "add", "--quiet", "-b", "release-fixture", worktree, "HEAD")
	for _, opts := range []Options{
		{Source: root, Output: gitConfig, Build: true},
		{Source: root, Output: filepath.Join(alias, "config"), Build: true},
		{Source: worktree, Output: gitConfig, Build: true},
		{Source: worktree, Output: filepath.Join(worktree, ".git"), Build: true},
		{Source: root, Output: filepath.Join(root, ".git", "new-parent", "package"), Snapshot: true},
		{Source: root, Output: filepath.Join(root, ".git", "new-parent", "cross"), CrossBuild: true},
	} {
		compiler := &testCompiler{}
		if _, err := run(context.Background(), opts, compiler.command); err == nil || !strings.Contains(err.Error(), "Git metadata") || compiler.builds != 0 {
			t.Fatalf("Git metadata destination accepted: %+v error=%v builds=%d", opts, err, compiler.builds)
		}
	}
	if string(releaseRead(t, gitConfig)) != before {
		t.Fatal("Git config was overwritten")
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "new-parent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected output created directories inside Git metadata: %v", err)
	}
}

func TestReleasePublicationDoesNotReplaceEmptyDestination(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "staged"), filepath.Join(root, "concurrent-owner")
	releaseMust(t, os.Mkdir(source, 0o700))
	releaseMust(t, os.WriteFile(filepath.Join(source, "artifact"), []byte("staged"), 0o600))
	releaseMust(t, os.Mkdir(destination, 0o700))
	if err := publishDirectory(source, destination); err == nil {
		t.Fatal("publication replaced an existing empty directory")
	}
	entries, err := os.ReadDir(destination)
	releaseMust(t, err)
	if len(entries) != 0 || string(releaseRead(t, filepath.Join(source, "artifact"))) != "staged" {
		t.Fatal("failed no-replace publication changed source or destination")
	}
}

func TestCrossBuildRefusesSymlinkOutputDirectory(t *testing.T) {
	root := releaseTestRepo(t)
	gitDir := filepath.Join(root, ".git")
	before := string(releaseRead(t, filepath.Join(gitDir, "config")))
	alias := filepath.Join(t.TempDir(), "cross-output")
	releaseMust(t, os.Symlink(gitDir, alias))
	compiler := &testCompiler{}
	_, err := run(context.Background(), Options{Source: root, Output: alias, CrossBuild: true}, compiler.command)
	if err == nil || !strings.Contains(err.Error(), "symlink") || compiler.builds != 0 {
		t.Fatalf("cross-build followed output symlink: error=%v builds=%d", err, compiler.builds)
	}
	if string(releaseRead(t, filepath.Join(gitDir, "config"))) != before {
		t.Fatal("cross-build changed Git metadata through output alias")
	}
	staging, err := filepath.Glob(filepath.Join(gitDir, ".csc-release-*"))
	releaseMust(t, err)
	if len(staging) != 0 {
		t.Fatalf("cross-build staged artifacts inside Git metadata: %v", staging)
	}
	for _, target := range releaseTargets {
		if _, err := os.Stat(filepath.Join(gitDir, rawName(target))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cross-build published inside Git metadata: %s (%v)", rawName(target), err)
		}
	}
}

package release

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type commandRunner func(context.Context, string, []string, string, ...string) ([]byte, error)

func runCommand(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	body, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return body, nil
}

func gitEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0")
}

func buildEnvironment(target Target) []string {
	// Preserve network/cache settings, but never ambient target, compiler,
	// experiment, workspace, persisted GOENV, or flag overrides.
	safeGo := map[string]bool{"GOPATH": true, "GOCACHE": true, "GOMODCACHE": true,
		"GOPROXY": true, "GOSUMDB": true, "GOPRIVATE": true, "GONOPROXY": true, "GONOSUMDB": true}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if (strings.HasPrefix(key, "GO") && !safeGo[key]) || strings.HasPrefix(key, "CGO_") || key == "CC" || key == "CXX" || key == "GCCGO" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOTOOLCHAIN="+toolchain, "GOENV=off", "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0")
	if target.OS != "" {
		env = append(env, "GOOS="+target.OS, "GOARCH="+target.Arch)
		if target.Arch == "amd64" {
			env = append(env, "GOAMD64=v1")
		}
		if target.Arch == "arm64" {
			env = append(env, "GOARM64=v8.0")
		}
	}
	return env
}

func checkToolchain(ctx context.Context, dir string, command commandRunner) error {
	body, err := command(ctx, dir, buildEnvironment(Target{}), "go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) != toolchain {
		return fmt.Errorf("release compiler must be %s, got %q", toolchain, strings.TrimSpace(string(body)))
	}
	return nil
}

func compile(ctx context.Context, root, output string, target Target, metadata Manifest, command commandRunner) error {
	ldflags := "-buildid= -X " + module + "/internal/bridge.Version=" + metadata.Version +
		" -X " + module + "/internal/bridge.BuildCommit=" + metadata.SourceCommit +
		" -X " + module + "/internal/bridge.BuildDate=" + metadata.BuildDate
	_, err := command(ctx, root, buildEnvironment(target), "go", "build", "-mod=readonly", "-trimpath",
		"-buildvcs=false", "-ldflags", ldflags, "-o", output, "./cmd/cross-session-codex")
	return err
}

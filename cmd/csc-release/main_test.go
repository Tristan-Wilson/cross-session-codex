package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelpDoesNotPrepareArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // Help must not need Git, Go, or a source checkout.
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{
		"--source", filepath.Join(dir, "missing-source"),
		"--out", filepath.Join(dir, "artifacts"), "--help",
	}, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "Usage of csc-release") {
		t.Fatalf("unexpected help output: stdout=%q stderr=%q", &out, &errOut)
	}
	assertNoPreparedArtifacts(t, dir)
}

func TestRunRejectsInvalidArgumentsBeforePreparation(t *testing.T) {
	const modeError = "choose exactly one"
	for _, test := range []struct {
		name   string
		args   []string
		output bool
		want   string
	}{
		{"unknown flag", []string{"--unknown"}, true, "flag provided but not defined"},
		{"forged version", []string{"--version", "v9.9.9"}, true, "flag provided but not defined"},
		{"forged commit", []string{"--commit", "forged"}, true, "flag provided but not defined"},
		{"forged build date", []string{"--build-date", "today"}, true, "flag provided but not defined"},
		{"compiler override", []string{"--go", "/different/compiler"}, true, "flag provided but not defined"},
		{"trailing argument", []string{"--snapshot", "unexpected"}, true, "unexpected positional arguments"},
		{"missing mode", nil, true, modeError},
		{"missing output", []string{"--snapshot"}, false, modeError},
		{"missing mode and output", nil, false, modeError},
		{"two build modes", []string{"--build", "--cross-build"}, true, modeError},
		{"build and snapshot", []string{"--build", "--snapshot"}, true, modeError},
		{"tag and snapshot", []string{"--tag", "v1.2.3", "--snapshot"}, true, modeError},
		{"incomplete tag", []string{"--tag", "v1.2"}, true, "complete v-prefixed semantic version"},
		{"tag without prefix", []string{"--tag", "1.2.3"}, true, "complete v-prefixed semantic version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			// No external tools are available, and the source does not exist.
			// The requested validation error must precede any build preparation.
			t.Setenv("PATH", dir)
			args := []string{"--source", filepath.Join(dir, "missing-source")}
			if test.output {
				args = append(args, "--out", filepath.Join(dir, "artifacts"))
			}
			args = append(args, test.args...)
			var out, errOut bytes.Buffer
			err := run(context.Background(), args, &out, &errOut)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q; stderr=%q", err, test.want, &errOut)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid invocation emitted a manifest: %s", &out)
			}
			assertNoPreparedArtifacts(t, dir)
		})
	}
}

func assertNoPreparedArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("CLI preparation wrote files: %v", entries)
	}
}

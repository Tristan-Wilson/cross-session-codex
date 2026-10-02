package bridge

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVersionReportsBuildStampWithoutRuntimeState(t *testing.T) {
	previousVersion, previousCommit, previousDate := Version, BuildCommit, BuildDate
	t.Cleanup(func() {
		Version, BuildCommit, BuildDate = previousVersion, previousCommit, previousDate
	})
	Version = "v1.2.3-rc.1"
	BuildCommit = "0123456789abcdef0123456789abcdef01234567"
	BuildDate = "2026-09-27T12:00:00Z"
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("CROSS_SESSION_CODEX_STATE_DIR", filepath.Join(t.TempDir(), "nonexistent"))
	var out, stderr bytes.Buffer
	if code := Main([]string{"version"}, bytes.NewReader(nil), &out, &stderr); code != 0 {
		t.Fatalf("version exit=%d stderr=%s", code, stderr.String())
	}
	var result map[string]string
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"name": "cross-session-codex", "version": Version,
		"commit": BuildCommit, "build_date": BuildDate,
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	} {
		if got := result[key]; got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	resultCaps, err := runCLI([]string{"capabilities"}, bytes.NewReader(nil), &out, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if str(resultCaps, "version") != Version || resultCaps["cli_api"] != 1 {
		t.Fatalf("capabilities stamp/interface=%v", resultCaps)
	}
}

package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestBuildEnvDisablesBackgroundNetworkWithoutDuplicatingOverrides(t *testing.T) {
	parent := []string{"KEEP=yes", "PI_CODING_AGENT_DIR=/native", "PI_OFFLINE=0", "PI_SKIP_VERSION_CHECK=0", "PI_TELEMETRY=1"}
	for _, dir := range []string{"", "/gateway"} {
		env := BuildEnv(parent, dir)
		values := map[string]string{}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			if _, exists := values[key]; exists {
				t.Fatalf("duplicate environment key %s", key)
			}
			values[key] = value
		}
		wantDir := dir
		if wantDir == "" {
			wantDir = "/native"
		}
		for key, want := range map[string]string{"KEEP": "yes", "PI_CODING_AGENT_DIR": wantDir, "PI_OFFLINE": "1", "PI_SKIP_VERSION_CHECK": "1", "PI_TELEMETRY": "0"} {
			if values[key] != want {
				t.Fatalf("%s = %q, want %q", key, values[key], want)
			}
		}
	}
}

func TestConfigDirUsesWingmanHome(t *testing.T) {
	home := testenv.WingmanHome(t)

	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "pi")
	if got != want {
		t.Fatalf("ConfigDir() = %q, want %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("ConfigDir() did not create %q: %v", want, err)
	}
}

func TestNativeSessionsDirUsesPiConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)

	want := filepath.Join(dir, "sessions")
	if got := NativeSessionsDir(); got != want {
		t.Fatalf("NativeSessionsDir() = %q, want %q", got, want)
	}
}

func TestNativeSessionsDirDefaultsUnderHome(t *testing.T) {
	home := testenv.UserHome(t)
	t.Setenv("PI_CODING_AGENT_DIR", "")

	want := filepath.Join(home, ".pi", "agent", "sessions")
	if got := NativeSessionsDir(); got != want {
		t.Fatalf("NativeSessionsDir() = %q, want %q", got, want)
	}
}

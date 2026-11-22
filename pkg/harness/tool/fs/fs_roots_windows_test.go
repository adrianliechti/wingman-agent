package fs

import (
	"strings"
	"testing"
)

func TestAllowedRootCanBeOnDifferentWindowsVolume(t *testing.T) {
	workspace := `C:\work\project`
	systemRoot := `D:\Wingman\skills\.system`
	skillFile := `D:\Wingman\skills\.system\feature-dev\SKILL.md`

	if _, ok := relPathWithinWorkspace(skillFile, workspace); ok {
		t.Fatalf("%q was treated as inside workspace %q", skillFile, workspace)
	}
	root, sub, ok := matchAllowedRoot(skillFile, []string{systemRoot})
	if !ok {
		t.Fatalf("%q did not match allowed root %q", skillFile, systemRoot)
	}
	if root != systemRoot || sub != `feature-dev\SKILL.md` {
		t.Fatalf("allowed-root match = (%q, %q), want (%q, %q)", root, sub, systemRoot, `feature-dev\SKILL.md`)
	}
}

func TestUnconfinedReadsStayOnLocalDrives(t *testing.T) {
	for path, local := range map[string]bool{
		`C:\work\project\main.go`:      true,
		`\\server\share\main.go`:       false,
		`\\?\UNC\server\share\main.go`: false,
		`\\?\C:\work\main.go`:          false,
		`\\.\pipe\wingman`:             false,
	} {
		if got := onLocalVolume(path); got != local {
			t.Errorf("onLocalVolume(%q) = %v, want %v", path, got, local)
		}
	}
	// A device path fails fast; a UNC path would wait for the name to resolve.
	workspace := t.TempDir()
	if _, err := matchReadTarget(`\\.\pipe\wingman-missing`, workspace, nil, "read file"); err == nil || !strings.Contains(err.Error(), "outside workspace and allowed roots") {
		t.Fatalf("unconfined device path = %v, want the outside-roots error", err)
	}
}

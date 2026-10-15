package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureRegistry = `[{"name":"custom","markers":["*.customproj"],"servers":[{"name":"custom-server","command":"custom-server","args":["--stdio"],"languages":["custom"],"language_id":"custom","extension_to_language":{".custom":"custom-script"}}]}]`

func TestRegistrySharesDetectionAndInstallationRequirements(t *testing.T) {
	registry, err := ParseRegistry([]byte(fixtureRegistry))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "app.customproj"), nil, 0600)
	requirements := registry.DetectRequirements(root)
	if len(requirements) != 1 || requirements[0].Commands[0] != "custom-server" || requirements[0].Directories[0] != root {
		t.Fatalf("requirements=%+v", requirements)
	}
	if roots := detectAll(root, nil, registry); len(roots) != 0 {
		t.Fatal("registry bypassed managed installation")
	}
	managed := filepath.Join(t.TempDir(), "custom-server")
	manager := NewManager(root, WithRegistry(registry), WithManagedTools(testManagedTools{"custom-server": managed}))
	defer manager.Close()
	server := manager.FindServer(filepath.Join(root, "main.custom"))
	if server == nil || server.Command != managed || server.LanguageIDForPath("main.custom") != "custom-script" {
		t.Fatalf("server=%+v", server)
	}
	server.Args[0] = "changed"
	server.ExtensionToLanguage[".custom"] = "changed"
	server = manager.FindServer(filepath.Join(root, "main.custom"))
	if server.Args[0] != "--stdio" || server.LanguageIDForPath("main.custom") != "custom-script" {
		t.Fatal("returned descriptor mutated immutable registry/cache")
	}
	if roots := detectAll(root, testManagedTools{"custom-server": managed}); len(roots) != 0 {
		t.Fatal("registry leaked globally")
	}
}

func TestRegistryRejectsInvalidOrOverridingDefinitions(t *testing.T) {
	for _, data := range []string{"null", fixtureRegistry + " []", strings.Replace(fixtureRegistry, `"custom","markers"`, `"go","markers"`, 1), strings.Replace(fixtureRegistry, `"command":"custom-server"`, `"command":"/tmp/server"`, 1), strings.Replace(fixtureRegistry, `"*.customproj"`, `"../marker"`, 1), strings.Replace(fixtureRegistry, `"*.customproj"`, `"["`, 1), strings.Replace(fixtureRegistry, `"language_id"`, `"unknown_field"`, 1)} {
		if _, err := ParseRegistry([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

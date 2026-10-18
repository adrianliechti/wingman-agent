package claudedesktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

const (
	profileName = "Wingman"
	profileID   = "00000000-0000-4000-8000-000000000424"
)

// Cowork needs unrestricted egress for user-configured plugins and MCP
// servers. This override only applies while the Wingman profile is active.
var coworkEgressAllowedHosts = []string{"*"}

type Options = external.Options

func Run(ctx context.Context, args []string, options *Options) error {
	restore, err := parseArgs(args)
	if err != nil {
		return err
	}

	if restore {
		if err := quitClaude(); err != nil {
			return err
		}
		if err := Restore(); err != nil {
			return err
		}

		fmt.Fprintln(os.Stderr, "Claude Desktop restored to the usual Claude profile.")
		return nil
	}

	if err := quitClaude(); err != nil {
		return err
	}
	if err := Configure(ctx, options); err != nil {
		return err
	}
	if err := openApp(); err != nil {
		_ = Restore()
		return err
	}

	fmt.Fprintln(os.Stderr, "Claude Desktop is running with Wingman. Press Ctrl-C to restore the usual Claude profile.")

	<-ctx.Done()

	fmt.Fprintln(os.Stderr, "Restoring Claude Desktop to the usual Claude profile.")

	if err := quitClaude(); err != nil {
		return err
	}
	if err := Restore(); err != nil {
		return err
	}

	return nil
}

func Configure(ctx context.Context, options *Options) error {
	if err := supported(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cfg := newGatewayConfig(options)

	targets, err := targetPaths()
	if err != nil {
		return err
	}

	return configureTargets(targets, cfg)
}

func configureTargets(targets targets, cfg gatewayConfig) error {
	target := targets.thirdPartyProfile
	// Prepare the inactive profile completely before switching Claude to it. A
	// malformed or unwritable profile must not strand Claude in third-party
	// mode with only part of the Wingman configuration present.
	if err := writeGatewayProfile(target.profile, cfg.BaseURL, cfg.AuthToken); err != nil {
		return err
	}
	if err := writeMeta(target.meta); err != nil {
		return err
	}
	if err := writeDeploymentMode(target.desktopConfig, "3p"); err != nil {
		return err
	}
	if err := writeDeploymentMode(targets.normalConfig, "3p"); err != nil {
		return err
	}

	return nil
}

type gatewayConfig struct {
	BaseURL   string
	AuthToken string
}

func newGatewayConfig(options *Options) gatewayConfig {
	options = external.WithDefaults(options)

	return gatewayConfig{
		BaseURL:   options.WingmanURL,
		AuthToken: options.WingmanToken,
	}
}

func Restore() error {
	if err := supported(); err != nil {
		return err
	}

	targets, err := targetPaths()
	if err != nil {
		return err
	}

	if err := writeDeploymentMode(targets.normalConfig, "1p"); err != nil {
		return err
	}

	target := targets.thirdPartyProfile
	if err := writeDeploymentMode(target.desktopConfig, "1p"); err != nil {
		return err
	}
	if err := restoreMeta(target.meta); err != nil {
		return err
	}
	if err := restoreProfile(target.profile); err != nil {
		return err
	}

	return nil
}

func parseArgs(args []string) (restore bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--restore":
			restore = true
		default:
			return false, fmt.Errorf("claude-desktop does not accept argument %q", arg)
		}
	}

	return restore, nil
}

type thirdPartyPaths struct {
	desktopConfig string
	meta          string
	profile       string
}

type targets struct {
	normalConfig      string
	thirdPartyProfile thirdPartyPaths
}

func targetPaths() (targets, error) {
	normalRoot, thirdPartyRoot, err := profileRoots()
	if err != nil {
		return targets{}, err
	}

	return targets{
		normalConfig: filepath.Join(normalRoot, "claude_desktop_config.json"),
		thirdPartyProfile: thirdPartyPaths{
			desktopConfig: filepath.Join(thirdPartyRoot, "claude_desktop_config.json"),
			meta:          filepath.Join(thirdPartyRoot, "configLibrary", "_meta.json"),
			profile:       filepath.Join(thirdPartyRoot, "configLibrary", profileID+".json"),
		},
	}, nil
}

func writeDeploymentMode(path, mode string) error {
	cfg, err := readJSONAllowMissing(path)
	if err != nil {
		return fmt.Errorf("parse Claude Desktop config: %w", err)
	}

	cfg["deploymentMode"] = mode
	return writeJSON(path, cfg)
}

func writeMeta(path string) error {
	meta, err := readJSONAllowMissing(path)
	if err != nil {
		return fmt.Errorf("parse Claude Desktop config metadata: %w", err)
	}

	meta["appliedId"] = profileID

	entries := make([]any, 0)
	for _, entry := range anySlice(meta["entries"]) {
		entryMap, _ := entry.(map[string]any)
		if entryMap == nil {
			entries = append(entries, entry)
			continue
		}
		if entryID, _ := entryMap["id"].(string); entryID == profileID {
			continue
		}
		entries = append(entries, entryMap)
	}

	entries = append(entries, map[string]any{
		"id":   profileID,
		"name": profileName,
	})
	meta["entries"] = entries

	return writeJSON(path, meta)
}

// gatewayProfile returns every setting Wingman owns; restore removes exactly these keys.
func gatewayProfile(baseURL, authToken string) map[string]any {
	return map[string]any{
		"inferenceProvider":            "gateway",
		"inferenceGatewayBaseUrl":      strings.TrimRight(baseURL, "/"),
		"inferenceGatewayApiKey":       authToken,
		"inferenceGatewayAuthScheme":   "bearer",
		"inferenceCredentialKind":      "static",
		"modelDiscoveryEnabled":        true,
		"modelCatalogEnabled":          false,
		"deploymentOrganizationUuid":   profileID,
		"deploymentDisplayName":        profileName,
		"disableDeploymentModeChooser": true,
		"chatTabEnabled":               true,
		"coworkEgressAllowedHosts":     coworkEgressAllowedHosts,
		"disableEssentialTelemetry":    true,
		"disableNonessentialTelemetry": true,
		"disableFeatureDiscovery":      true,
		"skipWebFetchPreflight":        true,
		// Desktop sessions lack CLAUDE_CODE_AUTO_MODE_SERVER=0, so classifier calls would leave the gateway API path.
		"autoModeEnabled": false,
	}
}

func writeGatewayProfile(path, baseURL, authToken string) error {
	cfg, err := readJSONAllowMissing(path)
	if err != nil {
		return fmt.Errorf("parse Claude Desktop Wingman profile: %w", err)
	}

	maps.Copy(cfg, gatewayProfile(baseURL, authToken))
	delete(cfg, "inferenceModels")

	return writeJSON(path, cfg)
}

func restoreMeta(path string) error {
	meta, err := readJSONAllowMissing(path)
	if err != nil {
		return fmt.Errorf("parse Claude Desktop config metadata: %w", err)
	}
	if len(meta) == 0 {
		return nil
	}

	changed := false
	if appliedID, _ := meta["appliedId"].(string); appliedID == profileID {
		delete(meta, "appliedId")
		changed = true
	}

	if entries := anySlice(meta["entries"]); entries != nil {
		filtered := make([]any, 0, len(entries))
		for _, entry := range entries {
			entryMap, _ := entry.(map[string]any)
			if entryID, _ := entryMap["id"].(string); entryID == profileID {
				changed = true
				continue
			}
			filtered = append(filtered, entry)
		}
		meta["entries"] = filtered
	}

	if !changed {
		return nil
	}

	return writeJSON(path, meta)
}

func restoreProfile(path string) error {
	cfg, err := readJSONAllowMissing(path)
	if err != nil {
		return fmt.Errorf("parse Claude Desktop Wingman profile: %w", err)
	}
	if len(cfg) == 0 {
		return nil
	}

	for key := range gatewayProfile("", "") {
		delete(cfg, key)
	}
	delete(cfg, "inferenceModels")
	cfg["disableDeploymentModeChooser"] = false

	return writeJSON(path, cfg)
}

func readJSONAllowMissing(path string) (map[string]any, error) {
	cfg, err := readJSON(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	return cfg, err
}

func readJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	return cfg, nil
}

func writeJSON(path string, cfg any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	mode := os.FileMode(0644)
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(path+".bak", existing, mode); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".wingman-claude-desktop-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

func anySlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case nil:
		return nil
	default:
		return nil
	}
}

func quitClaude() error {
	if !isRunning() {
		return nil
	}
	if err := quitApp(); err != nil {
		if !isRunning() {
			return nil
		}
		return fmt.Errorf("quit Claude Desktop: %w", err)
	}

	return waitForExit(30 * time.Second)
}

func waitForExit(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isRunning() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("Claude Desktop did not quit; quit it manually and re-run the command")
}

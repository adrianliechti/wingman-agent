package code

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

// approveMCP runs under mcpCatalogMu. Approval is bound to the exact loaded
// configuration and working directory, and stored outside the repository.
func (w *Workspace) approveMCP(ctx context.Context, ui UI, name string, server mcp.ServerConfig) error {
	config, err := json.Marshal(struct {
		Workspace string
		Name      string
		Source    string
		Directory string
		Config    mcp.ServerConfig
	}{w.RootPath, name, w.MCP.TrustRequired[name], server.Dir, server})
	if err != nil {
		return err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(config))
	if w.mcpTrustDecisions == nil {
		w.mcpTrustDecisions = map[string]bool{}
	}
	if allowed, ok := w.mcpTrustDecisions[fingerprint]; ok {
		if allowed {
			return nil
		}
		return fmt.Errorf("MCP server %s was not approved; restart the workspace to reconsider", name)
	}
	path := filepath.Join(projectStateDir(w.RootPath), "mcp-trust.json")
	trusted := map[string]bool{}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(data, &trusted); err != nil {
			return fmt.Errorf("read MCP approvals: %w", err)
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("read MCP approvals: %w", readErr)
	}
	if trusted == nil {
		trusted = map[string]bool{}
	}
	if trusted[fingerprint] {
		w.mcpTrustDecisions[fingerprint] = true
		return nil
	}
	if ui == nil {
		return fmt.Errorf("MCP server %s needs approval of %s before it can connect", name, w.MCP.TrustRequired[name])
	}
	description := server.URL
	if server.Command != "" {
		description = strings.Join(append([]string{server.Command}, server.Args...), " ")
	}
	dir := server.Dir
	if dir == "" {
		dir = w.RootPath
	}
	message := fmt.Sprintf("Allow MCP server %q from %s?\n\n%s\nWorking directory: %s\n\nThis configuration can execute code or contact its server. Approval is remembered for this exact configuration.", name, w.MCP.TrustRequired[name], description, dir)
	if len(server.Env) > 0 {
		message += "\nEnvironment overrides: " + strings.Join(slices.Sorted(maps.Keys(server.Env)), ", ")
	}
	allowed, err := ui.Confirm(ctx, message)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !allowed {
		w.mcpTrustDecisions[fingerprint] = false
		return fmt.Errorf("MCP server %s was not approved", name)
	}
	trusted[fingerprint] = true
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(trusted)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".mcp-trust-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	w.mcpTrustDecisions[fingerprint] = true
	return nil
}

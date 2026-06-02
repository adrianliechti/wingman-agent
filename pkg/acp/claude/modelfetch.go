package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/coder/acp-go-sdk"
)

// fetchModels asks the `claude` CLI for its real model list via the stdio
// control protocol. We spawn the CLI in streaming mode (the same transport a
// turn uses), send a single `initialize` control request, read the matching
// control response, and close stdin so the process exits. This replaces any
// hardcoded model table: the list always reflects what the installed CLI and
// the authenticated account actually offer.
func fetchModels(ctx context.Context, path string, env []string) ([]ModelEntry, []acp.AvailableCommand, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path,
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
	)
	if env != nil {
		cmd.Env = env
	}
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("start claude: %w", err)
	}
	// Closing stdin makes the CLI exit once it has answered the control
	// request, so the subprocess never lingers. cancel() is the hard backstop.
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	const reqID = "wingman-init"
	req := initControlRequest{Type: "control_request", RequestID: reqID}
	req.Request.Subtype = "initialize"
	b, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		return nil, nil, fmt.Errorf("write initialize: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp initControlResponse
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		if resp.Type != "control_response" || resp.Response.RequestID != reqID {
			continue
		}
		if resp.Response.Subtype != "success" {
			return nil, nil, fmt.Errorf("claude initialize failed: %s", resp.Response.Subtype)
		}
		return modelsFromCLI(resp.Response.Response.Models), commandsFromCLI(resp.Response.Response.Commands), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read claude init: %w", err)
	}
	return nil, nil, fmt.Errorf("claude closed without an initialize response")
}

// modelsFromCLI maps the CLI's reported models to picker entries. Effort levels
// are surfaced only for models that support them, so the effort selector is
// hidden (e.g. Haiku) where the CLI reports none.
func modelsFromCLI(list []cliModel) []ModelEntry {
	out := make([]ModelEntry, 0, len(list))
	for _, m := range list {
		name := m.DisplayName
		if name == "" {
			name = m.Value
		}
		var efforts []string
		if m.SupportsEffort {
			efforts = m.SupportedEffortLevels
		}
		out = append(out, ModelEntry{
			ID:           m.Value,
			Name:         name,
			Description:  m.Description,
			EffortLevels: efforts,
		})
	}
	return out
}

// commandsFromCLI maps the CLI's reported slash commands to ACP available
// commands. The argument hint, when present, becomes an unstructured input hint.
// hiddenCommands are CLI slash commands with no useful ACP behavior; the
// reference filters the same set (getAvailableSlashCommands).
var hiddenCommands = map[string]bool{
	"clear": true, "cost": true, "keybindings-help": true, "login": true,
	"logout": true, "output-style:new": true, "release-notes": true, "todos": true,
}

func commandsFromCLI(list []cliCommand) []acp.AvailableCommand {
	out := make([]acp.AvailableCommand, 0, len(list))
	for _, c := range list {
		if c.Name == "" || hiddenCommands[c.Name] {
			continue
		}
		cmd := acp.AvailableCommand{Name: c.Name, Description: c.Description}
		if c.ArgumentHint != "" {
			cmd.Input = &acp.AvailableCommandInput{Unstructured: &acp.UnstructuredCommandInput{Hint: c.ArgumentHint}}
		}
		out = append(out, cmd)
	}
	return out
}

// Control-protocol wire types for the `initialize` request/response. Only the
// fields we consume are modeled; the CLI emits many more (agents, account,
// output styles).
type initControlRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
}

type initControlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Models   []cliModel   `json:"models"`
			Commands []cliCommand `json:"commands"`
		} `json:"response"`
	} `json:"response"`
}

type cliCommand struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argumentHint"`
}

type cliModel struct {
	Value                 string   `json:"value"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

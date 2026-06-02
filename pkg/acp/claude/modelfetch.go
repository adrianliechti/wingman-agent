package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// fetchModels asks the `claude` CLI for its real model list via the stdio
// control protocol. We spawn the CLI in streaming mode (the same transport a
// turn uses), send a single `initialize` control request, read the matching
// control response, and close stdin so the process exits. This replaces any
// hardcoded model table: the list always reflects what the installed CLI and
// the authenticated account actually offer.
func fetchModels(ctx context.Context, path string, env []string) ([]ModelEntry, error) {
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
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start claude: %w", err)
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
		return nil, err
	}
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("write initialize: %w", err)
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
			return nil, fmt.Errorf("claude initialize failed: %s", resp.Response.Subtype)
		}
		return modelsFromCLI(resp.Response.Response.Models), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read claude init: %w", err)
	}
	return nil, fmt.Errorf("claude closed without an initialize response")
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

// Control-protocol wire types for the `initialize` request/response. Only the
// fields we consume are modeled; the CLI emits many more (commands, agents,
// account, output styles).
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
			Models []cliModel `json:"models"`
		} `json:"response"`
	} `json:"response"`
}

type cliModel struct {
	Value                 string   `json:"value"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

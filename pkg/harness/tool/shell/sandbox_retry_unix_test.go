//go:build darwin || linux

package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// A run that only lacked the network is retried inside the sandbox with
// network access. Full escalation is kept for what that does not fix and for
// commands that need the user's credentials.
func TestExecRetriesOfflineDenialWithNetworkFirst(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	outside := filepath.Join(home, fmt.Sprintf(".wingman-network-retry-%d", os.Getpid()))
	const offline = `echo "curl: (6) Could not resolve host: registry.invalid"; exit 6`

	for _, tc := range []struct {
		name    string
		command string
		deny    string // approval type the reviewer refuses
		kinds   []string
		runs    int
		outside bool
		notice  string
	}{
		{"network is enough", "echo run >> runs.txt; " + offline, "", []string{"sandbox_permissions"}, 2, false, "re-running the command with network access after approval"},
		{"network declined", "echo run >> runs.txt; " + offline, "sandbox_permissions", []string{"sandbox_permissions"}, 1, false, "Running it with network access was not approved"},
		{"still blocked with network", "echo run >> runs.txt; if [ -f marker ]; then touch " + escapeQuote(outside) + "; else : > marker; " + offline + "; fi",
			"", []string{"sandbox_permissions", "sandbox_escalation"}, 3, true, "re-running the command outside the sandbox after approval"},
		{"credentials go straight outside", "echo run >> runs.txt; git push 2>/dev/null; " + offline, "", []string{"sandbox_escalation"}, 2, false, "re-running the command outside the sandbox after approval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer os.Remove(outside)
			workspace := t.TempDir()
			var kinds, calls []string
			var networks []any
			elicit := &tool.Elicitation{Review: func(ctx context.Context, req tool.Approval) (tool.Verdict, error) {
				kind, _ := req.Action["type"].(string)
				kinds = append(kinds, kind)
				policy, _ := req.Action["execution_policy"].(map[string]any)
				networks = append(networks, policy["network"])
				call, _ := hook.ToolCallFromContext(ctx)
				calls = append(calls, call.Args)
				if kind == tc.deny {
					return tool.Verdict{Reason: "fixture denial"}, nil
				}
				return tool.Verdict{Allowed: true}, nil
			}}
			manager := NewExecManager(nil)
			defer manager.Close()
			args := map[string]any{"command": tc.command, "wait": 20}
			raw, _ := json.Marshal(args)
			ctx := hook.WithToolCall(context.Background(), tool.ToolCall{ID: "call-1", Name: "exec", Args: string(raw)})
			result, err := ExecTools(manager, workspace, elicit, nil)[0].Execute(ctx, args)
			if err != nil || strings.Contains(result.Content, "sandbox_apply: Operation not permitted") {
				t.Skipf("sandbox backend unavailable: %v %s", err, result.Content)
			}
			if strings.Join(kinds, ",") != strings.Join(tc.kinds, ",") {
				t.Fatalf("approvals = %v, want %v\n%s", kinds, tc.kinds, result.Content)
			}
			if !strings.Contains(result.Content, tc.notice) {
				t.Fatalf("result lacks %q:\n%s", tc.notice, result.Content)
			}
			if runs, _ := os.ReadFile(filepath.Join(workspace, "runs.txt")); strings.Count(string(runs), "run") != tc.runs {
				t.Fatalf("command ran %d times, want %d", strings.Count(string(runs), "run"), tc.runs)
			}
			if _, err := os.Stat(outside); (err == nil) != tc.outside {
				t.Fatalf("write outside the sandbox: %v", err)
			}
			// The network grant keeps the sandbox, and hooks and the reviewer see the call as it runs.
			if tc.kinds[0] == "sandbox_permissions" {
				if networks[0] != true || !strings.Contains(calls[0], `"sandbox_permissions":"require_permissions"`) || !strings.Contains(calls[0], `"network":true`) {
					t.Fatalf("network retry was reviewed as %v / %s", networks[0], calls[0])
				}
			}
		})
	}
}

// A command reviewed before it runs is not reviewed again when the sandbox
// blocks it: the first request says the approval covers that re-run, and the
// second is marked as covered for the host to pass without the reviewer.
func TestReviewedCommandIsNotReviewedAgainAfterDenial(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	home := sandboxTestHome(t)
	outside := filepath.Join(home, fmt.Sprintf(".wingman-covered-retry-%d", os.Getpid()))
	command := "git push --force 2>/dev/null; touch " + escapeQuote(outside)

	for _, validation := range []bool{false, true} {
		t.Run(fmt.Sprintf("validation=%t", validation), func(t *testing.T) {
			defer os.Remove(outside)
			type request struct {
				kind           string
				rerun, covered any
				message        string
			}
			var requests []request
			elicit := &tool.Elicitation{Review: func(_ context.Context, req tool.Approval) (tool.Verdict, error) {
				kind, _ := req.Action["type"].(string)
				policy, _ := req.Action["execution_policy"].(map[string]any)
				requests = append(requests, request{kind, policy["rerun_if_blocked"], req.Action["covered_by_review"], req.Message})
				return tool.Verdict{Allowed: true}, nil
			}}
			manager := NewExecManager(nil)
			defer manager.Close()
			args := map[string]any{"command": command, "wait": 20, "validation": validation}
			raw, _ := json.Marshal(args)
			ctx := hook.WithToolCall(context.Background(), tool.ToolCall{ID: "call-1", Name: "exec", Args: string(raw)})
			result, err := ExecTools(manager, t.TempDir(), elicit, nil)[0].Execute(ctx, args)
			if err != nil || strings.Contains(result.Content, "sandbox_apply: Operation not permitted") {
				t.Skipf("sandbox backend unavailable: %v %s", err, result.Content)
			}
			if validation {
				// A check is never re-run, so its approval promises nothing more.
				if len(requests) != 1 || requests[0].rerun != nil || strings.Contains(requests[0].message, "runs once more") {
					t.Fatalf("requests = %+v", requests)
				}
				return
			}
			if len(requests) != 2 {
				t.Fatalf("requests = %+v\n%s", requests, result.Content)
			}
			first, second := requests[0], requests[1]
			if first.kind != "shell_command" || first.rerun != true || first.covered != nil || !strings.Contains(first.message, "runs once more") {
				t.Errorf("first request does not announce the re-run: %+v", first)
			}
			if second.kind != "sandbox_escalation" || second.covered != true {
				t.Errorf("re-run is not marked as covered: %+v", second)
			}
			if _, err := os.Stat(outside); err != nil {
				t.Errorf("the covered re-run did not happen outside the sandbox: %v", err)
			}
		})
	}
}

func TestOfflineDenialAndCredentialCommands(t *testing.T) {
	for line, want := range map[string]bool{
		"curl: (6) Could not resolve host: example.com":                     true,
		"npm error getaddrinfo ENOTFOUND registry.npmjs.org":                true,
		"dial tcp: lookup proxy.golang.org: no such host":                   true,
		"touch: /etc/x: Operation not permitted":                            false,
		"ssh: connect to host github.com: Permission denied (publickey)":    false,
		"open /Users/x/.npm/_cacache: permission denied, getaddrinfo later": false,
		"--- FAIL: TestSomething":                                           false,
	} {
		if got := isOfflineDenial(line); got != want {
			t.Errorf("isOfflineDenial(%q) = %v, want %v", line, got, want)
		}
	}
	for command, want := range map[string]bool{
		"git push":                              true,
		"git push origin main --force":          true,
		"cd repo && git -C . fetch --all":       true,
		"git clone git@github.com:o/r.git":      true,
		"gh pr create --fill":                   true,
		"ssh host uptime":                       true,
		"rsync -a build/ host:/srv/":            true,
		"(scp a host:b)":                        true,
		"npm install":                           false,
		"go mod download":                       false,
		"git status && git commit -m 'push it'": false,
		"pip install -r requirements.txt":       false,
		"curl -fsSL https://example.com":        false,
		"echo gh":                               false,
		"./ssh-keygen-wrapper":                  false,
	} {
		if got := usesHostCredentials(command); got != want {
			t.Errorf("usesHostCredentials(%q) = %v, want %v", command, got, want)
		}
	}
}

// Package managers retry for a long time without a network; a sandbox without
// one tells them not to, unless the user configured the retries.
func TestOfflineSandboxStopsPackageManagerRetries(t *testing.T) {
	t.Setenv(workspaceSandboxActiveEnv, "")
	t.Setenv("PIP_RETRIES", "7")
	environment := func(opts *Options) map[string]string {
		workspace := t.TempDir()
		cmd, err := buildToolCommand(t.Context(), "true", workspace, workspace, opts, false)
		if err != nil {
			t.Skipf("sandbox backend unavailable: %v", err)
		}
		values := map[string]string{}
		for _, entry := range cmd.Env {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
		}
		return values
	}
	offline := environment(&Options{})
	if offline["npm_config_fetch_retries"] != "0" || offline["CARGO_NET_RETRY"] != "0" {
		t.Errorf("offline sandbox keeps package manager retries: npm=%q cargo=%q", offline["npm_config_fetch_retries"], offline["CARGO_NET_RETRY"])
	}
	if offline["PIP_RETRIES"] != "7" {
		t.Errorf("configured PIP_RETRIES was replaced with %q", offline["PIP_RETRIES"])
	}
	for name, values := range map[string]map[string]string{
		"network": environment(&Options{Network: true}),
		"off":     environment(&Options{DisableSandbox: true}),
	} {
		if _, set := values["npm_config_fetch_retries"]; set {
			t.Errorf("sandbox %s: retries were disabled although the network is available", name)
		}
	}
}

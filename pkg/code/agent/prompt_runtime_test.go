package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestSharedPromptReferencesRegisteredToolSchemas(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{Options: optionsFromEnvironment()}, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s := a.session(id)
	assertTools(t, a, id,
		[]string{"exec_command", "web_search", "ask", "task_output", "task_send", "task_stop"},
		[]string{"exec", "search_web", "elicit", "agent_output", "agent_send", "agent_stop"},
	)
	retiredNames := regexp.MustCompile(`\b(exec_command|web_search|task_output|task_send|task_stop)\b`)
	identifiers := map[string]bool{}
	var collectProperties func(any)
	collectProperties = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if properties, ok := value["properties"].(map[string]any); ok {
				for name := range properties {
					identifiers[name] = true
				}
			}
			for _, child := range value {
				collectProperties(child)
			}
		case []any:
			for _, child := range value {
				collectProperties(child)
			}
		}
	}
	for _, candidate := range s.harness.AvailableTools() {
		identifiers[candidate.Name] = true
		collectProperties(candidate.Parameters)
		parameters, err := json.Marshal(candidate.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		if stale := retiredNames.FindString(candidate.Description + string(parameters)); stale != "" {
			t.Errorf("%s still advertises retired tool %q", candidate.Name, stale)
		}
	}
	// Inspect actual shared prose against the registered schemas, including
	// nested edit and question parameters. Code-intelligence operation names
	// have a separate contract and are not tool names or parameter keys.
	instructions := prompt.BuildBaseInstructions("", prompt.SectionData{ToolNames: promptToolNames(s.harness.AvailableTools())})
	var checked strings.Builder
	for _, section := range strings.Split(instructions, "\n\n# ") {
		if !strings.HasPrefix(section, "Code intelligence\n") {
			checked.WriteString(section)
		}
	}
	for _, match := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(checked.String(), -1) {
		if !identifiers[match[1]] {
			t.Errorf("shared prompt references unregistered tool or parameter %q", match[1])
		}
	}

	// Use the real question tool rather than a fixture that can retain an old name.
	assertTools(t, a, id, nil, []string{"elicit"})
	if err := a.SetUnattended(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	assertTools(t, a, id, []string{"elicit"}, []string{"read"})
	if strings.Contains(s.instructions(), "`elicit`") {
		t.Fatal("unattended prompt still advertises elicit")
	}
}

func TestPromptGuidanceFollowsConfiguredTools(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	for _, tc := range []struct {
		name    string
		options Options
	}{
		{name: "all"},
		{name: "no shell", options: Options{DisableShell: true}},
		{name: "no search", options: Options{DisableWebSearch: true}},
		{name: "no fetch", options: Options{DisableWebFetch: true}},
		{name: "no web", options: Options{DisableWebSearch: true, DisableWebFetch: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(ws, Config{Options: tc.options}, nil)
			t.Cleanup(func() { _ = a.Close() })
			id, err := a.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s := a.session(id)
			for _, unattended := range []bool{false, true} {
				s.unattended.Store(unattended)
				got := s.instructions()
				for name, want := range map[string]bool{
					"exec": !tc.options.DisableShell, "exec_session": !tc.options.DisableShell,
					"search_web": !tc.options.DisableWebSearch, "fetch": !tc.options.DisableWebFetch,
					"elicit": !unattended, "agent": true,
				} {
					if strings.Contains(got, "`"+name+"`") != want {
						t.Errorf("prompt guidance for %s does not match configuration (unattended=%v)", name, unattended)
					}
				}
			}
		})
	}
}

func TestDelegatedRequestsIncludeRuntimeAndActiveMode(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	type request struct {
		Instructions string `json:"instructions"`
		Tools        []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	requests := make(chan request, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- req
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"review complete\"}]}]}}\n\n")
	}))
	t.Cleanup(provider.Close)
	t.Setenv("WINGMAN_URL", provider.URL)
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequireFinish = func(string) bool { return false }
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s := a.session(id)
	for _, tc := range []struct {
		name         string
		plan         bool
		unattended   bool
		role         string
		disableWeb   bool
		disableShell bool
	}{
		{name: "reviewer", role: "code-reviewer"},
		{name: "general purpose", role: "general-purpose"},
		{name: "plan", role: "code-reviewer", plan: true},
		{name: "unattended plan", role: "code-reviewer", plan: true, unattended: true},
		{name: "no web", role: "code-reviewer", disableWeb: true},
		{name: "no shell", role: "verification", disableShell: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.setMode(modeAgent)
			if tc.plan {
				s.setMode(modePlan)
			}
			s.unattended.Store(tc.unattended)
			originalTools := s.tools
			if tc.disableWeb || tc.disableShell {
				var filtered []tool.Tool
				for _, candidate := range s.tools {
					if tc.disableWeb && (candidate.Name == "search_web" || candidate.Name == "fetch") {
						continue
					}
					if tc.disableShell && (candidate.Name == "exec" || candidate.Name == "exec_session") {
						continue
					}
					filtered = append(filtered, candidate)
				}
				s.tools = filtered
				t.Cleanup(func() { s.tools = originalTools })
			}
			called := false
			for _, candidate := range s.harness.AvailableTools() {
				if candidate.Name != "agent" {
					continue
				}
				called = true
				result, err := candidate.Execute(t.Context(), map[string]any{"agent_type": tc.role, "description": "Review", "prompt": "Review the assigned code without editing it."})
				if err != nil || result.IsError {
					t.Fatalf("delegation: result=%+v err=%v", result, err)
				}
			}
			if !called {
				t.Fatal("agent tool missing")
			}
			select {
			case req := <-requests:
				got := req.Instructions
				for _, heading := range []string{"Wingman runtime", "Project Guidelines", "Environment"} {
					if strings.Count(got, "# "+heading+"\n") != 1 {
						t.Errorf("child request duplicates or omits %s", heading)
					}
				}
				if strings.Contains(got, "# Plan mode") != tc.plan || strings.Contains(got, "# Unattended operation") != tc.unattended {
					t.Error("child request lost active mode restrictions")
				}
				for _, want := range []string{"Preserve unrelated user changes", "If blocked, distinguish finished work from what remains"} {
					if !strings.Contains(got, want) {
						t.Errorf("child request missing %q", want)
					}
				}
				available := map[string]bool{}
				for _, candidate := range req.Tools {
					available[candidate.Name] = true
				}
				for _, name := range []string{"agent", "agent_output", "elicit", "edit", "exec", "search_web", "fetch", "lsp"} {
					if strings.Contains(got, "`"+name+"`") != available[name] {
						t.Errorf("child prompt guidance for %s does not match serialized tools", name)
					}
				}
				if available["agent"] || strings.Contains(got, "# Delegation") || strings.Contains(got, "shared delegation policy") {
					t.Error("child retained unavailable delegation capability")
				}
			default:
				t.Fatal("provider received no delegated request")
			}
		})
	}
}

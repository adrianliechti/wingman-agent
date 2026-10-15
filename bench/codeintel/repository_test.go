package codeintel

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	filetools "github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	graphtools "github.com/adrianliechti/wingman-agent/pkg/agent/tool/graph"
	lsptools "github.com/adrianliechti/wingman-agent/pkg/agent/tool/lsp"
	mcptools "github.com/adrianliechti/wingman-agent/pkg/agent/tool/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/graph"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

//go:embed repo_tasks.json
var repoTaskData []byte

//go:embed repos.json
var repoManifest []byte

type repositoryTask struct {
	Name     string         `json:"name"`
	Question string         `json:"question"`
	Answer   map[string]any `json:"answer"`
}

type repositoryTurn struct {
	trial
	ChecksPassed    int   `json:"checks_passed"`
	ChecksTotal     int   `json:"checks_total"`
	LastInputTokens int64 `json:"last_input_tokens"`
}

type repositorySession struct {
	Repository       string           `json:"repository"`
	Model            string           `json:"model"`
	Configuration    string           `json:"configuration"`
	Repeat           int              `json:"repeat"`
	SetupMS          int64            `json:"setup_ms"`
	Failure          string           `json:"failure,omitempty"`
	ToolSchemaSHA256 string           `json:"tool_schema_sha256,omitempty"`
	ToolNames        []string         `json:"tool_names,omitempty"`
	Turns            []repositoryTurn `json:"turns"`
}

var repositoryExternalLocks sync.Map

func repositoryTasks(t *testing.T) map[string][]repositoryTask {
	t.Helper()
	var tasks map[string][]repositoryTask
	if err := json.Unmarshal(repoTaskData, &tasks); err != nil {
		t.Fatal(err)
	}
	return tasks
}

// Scores facts, not prose similarity or the model's confidence. Additional
// keys, malformed/trailing JSON, and missing facts cannot earn a full pass.
func scoreRepository(task repositoryTask, answer string) (int, string) {
	answer = strings.TrimSpace(answer)
	if strings.HasPrefix(answer, "```") {
		if n := strings.IndexByte(answer, '\n'); n >= 0 {
			answer = strings.TrimSpace(strings.TrimSuffix(answer[n+1:], "```"))
		}
	}
	var got map[string]any
	decoder := json.NewDecoder(strings.NewReader(answer))
	if err := decoder.Decode(&got); err != nil {
		return 0, "invalid answer JSON: " + err.Error()
	}
	if decoder.Decode(new(any)) != io.EOF {
		return 0, "trailing answer data"
	}
	passed := 0
	for key, want := range task.Answer {
		if reflect.DeepEqual(got[key], want) {
			passed++
		}
	}
	if passed != len(task.Answer) || len(got) != len(task.Answer) {
		return passed, fmt.Sprintf("facts %d/%d; got %s", passed, len(task.Answer), answer)
	}
	return passed, ""
}

func TestRepositoryScoring(t *testing.T) {
	task := repositoryTasks(t)["gin"][2]
	for _, tc := range []struct {
		answer string
		passed int
		good   bool
	}{
		{`{"terminates_current_handler":false,"abort_index":63}`, 2, true},
		{`{"terminates_current_handler":true,"abort_index":63}`, 1, false},
		{`{"abort_index":63}`, 1, false},
		{`{"terminates_current_handler":false,"abort_index":63,"extra":true}`, 2, false},
		{`{"terminates_current_handler":false,"abort_index":63} {}`, 0, false},
		{`not JSON`, 0, false},
	} {
		passed, failure := scoreRepository(task, tc.answer)
		if passed != tc.passed || (failure == "") != tc.good {
			t.Fatalf("%s => %d %s", tc.answer, passed, failure)
		}
	}
}

func TestRepositoryGroundTruth(t *testing.T) {
	corpus := os.Getenv("CODEINTEL_CORPUS")
	if corpus == "" {
		t.Skip("set CODEINTEL_CORPUS to the pinned corpus")
	}
	// These declarations were inspected independently in the pinned source.
	// Protect exact-line questions from accidental version/fixture drift.
	for repo, tasks := range repositoryTasks(t) {
		for _, task := range tasks {
			for _, value := range task.Answer {
				location, ok := value.(string)
				if !ok || !strings.Contains(location, ":") {
					continue
				}
				file, number, _ := strings.Cut(location, ":")
				var line int
				fmt.Sscan(number, &line)
				data, err := os.ReadFile(filepath.Join(corpus, repo, file))
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(string(data), "\n")
				if line < 1 || line > len(lines) {
					t.Fatal(location)
				}
				text := strings.TrimSpace(lines[line-1])
				if !strings.HasPrefix(text, "func ") && !strings.HasPrefix(text, "def ") && !strings.HasPrefix(text, "private ") && !strings.HasPrefix(text, "export const ") {
					t.Errorf("%s %s is not a declaration: %s", repo, location, text)
				}
			}
		}
	}
}

// The two qualified names repeatedly failed in the real model traces.
func TestRepositoryQueryRecovery(t *testing.T) {
	corpus := os.Getenv("CODEINTEL_CORPUS")
	if corpus == "" {
		t.Skip("set CODEINTEL_CORPUS to the pinned corpus")
	}
	for _, tc := range []struct {
		repo, query, accepted, target string
	}{
		{"gin", "Engine.ServeHTTP", "gin.go:ServeHTTP", "context.go:Next"},
		{"vscode", "RPCProtocol._receiveRequest", "src/vs/workbench/services/extensions/common/rpcProtocol.ts:_receiveRequest", "src/vs/workbench/services/extensions/common/rpcProtocol.ts:_doInvokeHandler"},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			engine := graph.New(filepath.Join(corpus, tc.repo), filepath.Join(corpus, tc.repo+".graph.json"))
			_, err := engine.Trace(t.Context(), tc.query, "", "", false, 4)
			if err == nil || !strings.Contains(err.Error(), tc.accepted) {
				t.Fatalf("missing recovery candidate: %v", err)
			}
			t.Log(err)
			result, err := engine.Trace(t.Context(), tc.accepted, tc.target, "", false, 4)
			if err != nil || len(result.Paths) == 0 {
				t.Fatalf("suggested query cannot recover the known path: %+v, %v", result, err)
			}
		})
	}
}

// Opt-in: no network, installations, or inference in ordinary go test runs.
// All model configurations run inside the same Wingman agent implementation.
func TestRepositoryModelMatrix(t *testing.T) {
	corpus, models := os.Getenv("CODEINTEL_CORPUS"), os.Getenv("CODEINTEL_MODELS")
	if corpus == "" || models == "" {
		t.Skip("set CODEINTEL_CORPUS and CODEINTEL_MODELS")
	}
	if *repetitions < 1 || *reportPath == "" {
		t.Fatal("positive repetitions and -codeintel-report required")
	}
	managed, err := devtools.New()
	if err != nil {
		t.Fatal(err)
	}
	tasks := repositoryTasks(t)
	if filter := os.Getenv("CODEINTEL_REPOS"); filter != "" {
		selected := map[string][]repositoryTask{}
		for _, repo := range strings.Split(filter, ",") {
			if _, ok := tasks[repo]; !ok {
				t.Fatal("unknown repository", repo)
			}
			selected[repo] = tasks[repo]
		}
		tasks = selected
	}
	configs := []string{"search_read", "graph", "graph_lsp", "graph_missing_lsp", "external_graph"}
	if filter := os.Getenv("CODEINTEL_CONFIGS"); filter != "" {
		configs = strings.Split(filter, ",")
	}
	for _, cfg := range configs {
		if !slices.Contains([]string{"search_read", "graph", "graph_lsp", "graph_missing_lsp", "external_graph"}, cfg) {
			t.Fatal("unknown config", cfg)
		}
	}
	var receipts map[string]any = map[string]any{}
	var specs map[string]struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(repoManifest, &specs); err != nil {
		t.Fatal(err)
	}
	for repo := range tasks {
		data, err := os.ReadFile(filepath.Join(corpus, repo+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt["commit"] != specs[repo].Commit {
			t.Fatal("repository receipt differs from pinned manifest:", repo)
		}
		receipts[repo] = receipt
	}
	result := struct {
		Started        string              `json:"started"`
		Models         []string            `json:"models"`
		Configurations []string            `json:"configurations"`
		Repetitions    int                 `json:"repetitions"`
		Effort         string              `json:"effort"`
		Repositories   map[string]any      `json:"repositories"`
		TaskSHA256     string              `json:"task_sha256"`
		PromptSHA256   map[string]string   `json:"prompt_sha256"`
		Methodology    []string            `json:"methodology"`
		Sessions       []repositorySession `json:"sessions"`
	}{Started: time.Now().UTC().Format(time.RFC3339), Models: strings.Split(models, ","), Configurations: slices.Clone(configs), Repetitions: *repetitions, Effort: "low", Repositories: receipts, TaskSHA256: fmt.Sprintf("%x", sha256.Sum256(repoTaskData)), PromptSHA256: map[string]string{}, Methodology: []string{
		"Selected pinned public repository snapshots (the full suite has four); three related questions per session: structural discovery, follow-up, and simple factual lookup. Gold facts inspected in source; no graph or model generates the expected answers.",
		"Same Wingman harness and model-specific production prompt in all arms. Read/grep/glob available in every arm. No shell, delegation, web, edits, or access to gold answers. Configuration enumeration rotates; parallel scheduling is uncontrolled. Tool choice is free.",
		"Indexes prebuilt offline; graph cold build/rebuild measured separately. Identical structural index in all Wingman graph arms; LSP is an additional model tool, not an indexing oracle. Each session gets a new agent and LSP/MCP connection.",
		"External graph uses the pinned CodeGraph npm binary and the production Wingman MCP bridge. Only its eight read-only tools are exposed; server instructions are not injected (matching Wingman integration). No graph is accessible from baseline file tools.",
		"Read-only sources reused across sessions. External index lives in a separate source copy. Source dependencies are not installed; language servers may have partial project information. Missing-LSP arm deliberately has no managed servers.",
		"All setup failures, model failures, wrong answers and tool errors retained. Provider-reported cumulative tokens and final-request input tokens recorded; final-request input is not exact per-tool retained-context attribution. No context compaction configured.",
		"Small selected task set; per-model findings do not establish general rankings. Parallel trials share host load; caches and provider latency are uncontrolled. Schema hashes record availability overhead.",
	}}
	for _, model := range strings.Split(models, ",") {
		result.PromptSHA256[model] = fmt.Sprintf("%x", sha256.Sum256([]byte(prompt.BuildBaseInstructions(prompt.VariantFor(model).Agent, prompt.SectionData{}))))
	}
	var mu sync.Mutex
	save := func(session repositorySession) {
		mu.Lock()
		defer mu.Unlock()
		result.Sessions = append(result.Sessions, session)
		slices.SortFunc(result.Sessions, func(a, b repositorySession) int {
			return strings.Compare(fmt.Sprintf("%s/%s/%03d/%s", a.Model, a.Repository, a.Repeat, a.Configuration), fmt.Sprintf("%s/%s/%03d/%s", b.Model, b.Repository, b.Repeat, b.Configuration))
		})
		data, err := json.MarshalIndent(result, "", "  ")
		if err == nil {
			err = os.WriteFile(*reportPath+".tmp", append(data, '\n'), 0600)
		}
		if err == nil {
			err = os.Rename(*reportPath+".tmp", *reportPath)
		}
		if err != nil {
			t.Error(err)
		}
	}
	for _, model := range strings.Split(models, ",") {
		for ri, repo := range slices.Sorted(maps.Keys(tasks)) {
			for repeat := 1; repeat <= *repetitions; repeat++ {
				for ci := range configs {
					cfg := configs[(ci+ri+repeat-1)%len(configs)]
					t.Run(fmt.Sprintf("%s/%s/%d/%s", model, repo, repeat, cfg), func(t *testing.T) {
						t.Parallel()
						session := runRepositorySession(t, corpus, repo, model, cfg, repeat, tasks[repo], managed)
						save(session)
						passed := 0
						for _, turn := range session.Turns {
							if turn.Correct {
								passed++
							}
						}
						t.Logf("REPO model=%s repo=%s config=%s repeat=%d passed=%d/%d setup_ms=%d failure=%s", model, repo, cfg, repeat, passed, len(session.Turns), session.SetupMS, session.Failure)
						if passed != len(tasks[repo]) {
							t.Errorf("%d/%d answers correct; see retained report", passed, len(tasks[repo]))
						}
					})
				}
			}
		}
	}
}

func runRepositorySession(t *testing.T, corpus, repo, model, cfg string, repeat int, tasks []repositoryTask, managed *devtools.Manager) (result repositorySession) {
	result = repositorySession{Repository: repo, Model: model, Configuration: cfg, Repeat: repeat}
	started := time.Now()
	fail := func(err error) repositorySession {
		result.Failure = err.Error()
		result.SetupMS = time.Since(started).Milliseconds()
		for _, task := range tasks {
			result.Turns = append(result.Turns, repositoryTurn{trial: trial{Task: task.Name, Configuration: cfg, Repeat: repeat, Failure: "setup: " + err.Error()}, ChecksTotal: len(task.Answer)})
		}
		return result
	}
	root := filepath.Join(corpus, repo)
	// File tools always point at the clean source tree. The external server
	// alone sees its equivalent copy containing .codegraph artifacts.
	fsRoot, err := os.OpenRoot(root)
	if err != nil {
		return fail(err)
	}
	defer fsRoot.Close()
	var available []tool.Tool
	for _, candidate := range filetools.Tools(fsRoot, nil) {
		if slices.Contains([]string{"read", "grep", "glob"}, candidate.Name) {
			available = append(available, candidate)
		}
	}
	if strings.HasPrefix(cfg, "graph") {
		engine := graph.New(root, filepath.Join(corpus, repo+".graph.json"))
		if !engine.StatusOrLoad().Indexed {
			return fail(fmt.Errorf("missing offline graph for %s", repo))
		}
		available = append(available, graphtools.NewTools(engine)...)
	}
	if cfg == "graph_lsp" || cfg == "graph_missing_lsp" {
		var opts []lsp.ManagerOption
		if cfg == "graph_lsp" {
			opts = append(opts, lsp.WithManagedTools(managed))
		}
		service := language.New(root, "", opts...)
		defer service.Close()
		available = append(available, lsptools.NewTools(service)...)
	}
	if cfg == "external_graph" {
		// Direct MCP mode has one writer lock per index. Serialize sessions
		// sharing a prebuilt index rather than manufacture lock-contention failures.
		lock, _ := repositoryExternalLocks.LoadOrStore(repo, new(sync.Mutex))
		lock.(*sync.Mutex).Lock()
		defer lock.(*sync.Mutex).Unlock()
		started = time.Now()
		binary := os.Getenv("CODEINTEL_EXTERNAL_BIN")
		if binary == "" {
			return fail(fmt.Errorf("CODEINTEL_EXTERNAL_BIN is required"))
		}
		externalRoot := filepath.Join(corpus, "external", repo)
		manager := mcp.NewManager(&mcp.Config{Servers: map[string]mcp.ServerConfig{"external": {Command: binary, Args: []string{"serve", "--mcp", "--no-watch", "--path", externalRoot}, Dir: externalRoot, Env: map[string]string{"CODEGRAPH_NO_DAEMON": "1", "CODEGRAPH_TELEMETRY": "0", "DO_NOT_TRACK": "1"}}}})
		defer manager.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		if err := manager.Connect(ctx); err != nil {
			return fail(err)
		}
		external, err := mcptools.ToolsForServer(ctx, "external", manager.Session("external"))
		if err != nil {
			return fail(err)
		}
		for _, candidate := range external {
			if slices.Contains([]string{"external_codegraph_search", "external_codegraph_callers", "external_codegraph_callees", "external_codegraph_impact", "external_codegraph_node", "external_codegraph_explore", "external_codegraph_status", "external_codegraph_files"}, candidate.Name) {
				available = append(available, candidate)
			}
		}
		if len(available) == 3 {
			return fail(fmt.Errorf("external server exposed no expected tools"))
		}
	}
	slices.SortFunc(available, func(a, b tool.Tool) int { return strings.Compare(a.Name, b.Name) })
	var schema []any
	for _, candidate := range available {
		result.ToolNames = append(result.ToolNames, candidate.Name)
		schema = append(schema, map[string]any{"name": candidate.Name, "description": candidate.Description, "parameters": candidate.Parameters})
	}
	encoded, _ := json.Marshal(schema)
	result.ToolSchemaSHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
	var traceMu sync.Mutex
	var traces []trace
	for i, candidate := range available {
		execute := candidate.Execute
		available[i].Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
			start := time.Now()
			out, err := execute(ctx, args)
			op, _ := args["operation"].(string)
			event := trace{Name: candidate.Name, Operation: op, Args: args, ElapsedMS: time.Since(start).Milliseconds(), OutputBytes: len(out.Content), Output: out.Content}
			if err != nil {
				event.Error = err.Error()
			} else if out.IsError {
				event.Error = out.Content
			}
			traceMu.Lock()
			traces = append(traces, event)
			traceMu.Unlock()
			return out, err
		}
	}
	hcfg, err := harness.DefaultConfig()
	if err != nil {
		return fail(err)
	}
	hcfg.Model = func() string { return model }
	hcfg.Effort = func() string { return "low" }
	hcfg.MaxTurns = 20
	hcfg.ContextWindow = -1
	hcfg.Tools = func() []tool.Tool { return available }
	hcfg.Instructions = func() string {
		return prompt.BuildBaseInstructions(prompt.VariantFor(model).Agent, prompt.SectionData{}) + "\nWorkspace: " + root + "\nAnswer using this checkout, which can differ from versions you remember. Use the available tools; do not delegate or request new tools. Return only the requested JSON. Paths in answers must be relative to the repository root. The external graph, when available, indexes an identical source copy."
	}
	agent := &harness.Agent{Config: hcfg}
	result.SetupMS = time.Since(started).Milliseconds()
	for _, task := range tasks {
		before := agent.UsageSnapshot()
		messagesBefore := len(agent.MessagesSnapshot())
		traceMu.Lock()
		traceStart := len(traces)
		traceMu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		start := time.Now()
		stream, runErr := agent.Send(ctx, []harness.Content{{Text: task.Question}})
		if runErr == nil {
			for _, err := range stream {
				if err != nil {
					runErr = err
					break
				}
			}
		}
		cancel()
		turn := repositoryTurn{trial: trial{Task: task.Name, Configuration: cfg, Repeat: repeat, ElapsedMS: time.Since(start).Milliseconds(), Tools: map[string]int{}, Operations: map[string]int{}}, ChecksTotal: len(task.Answer)}
		messages := agent.MessagesSnapshot()
		for _, message := range messages[messagesBefore:] {
			var final strings.Builder
			for _, content := range message.Content {
				if call := content.ToolCall; call != nil {
					turn.ToolCalls++
					turn.Tools[call.Name]++
				}
				if content.ToolResult != nil && content.ToolResult.IsError {
					turn.ToolErrors++
				}
				if message.Role == harness.RoleAssistant {
					final.WriteString(content.Text)
				}
			}
			if final.Len() > 0 {
				turn.Final = final.String()
			}
		}
		usage := agent.UsageSnapshot()
		turn.InputTokens = usage.InputTokens - before.InputTokens
		turn.OutputTokens = usage.OutputTokens - before.OutputTokens
		turn.LastInputTokens = usage.LastInputTokens
		traceMu.Lock()
		turn.Trace = slices.Clone(traces[traceStart:])
		traceMu.Unlock()
		for _, event := range turn.Trace {
			if event.Operation != "" {
				turn.Operations[event.Name+"."+event.Operation]++
			}
		}
		turn.ChecksPassed, turn.Failure = scoreRepository(task, turn.Final)
		if runErr != nil {
			turn.Failure = runErr.Error()
		}
		turn.Correct = turn.Failure == ""
		result.Turns = append(result.Turns, turn)
	}
	return result
}

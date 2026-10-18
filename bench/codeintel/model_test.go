package codeintel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/graph"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

var reportPath = flag.String("codeintel-report", "", "write JSON measurements to this path (optional)")
var repetitions = flag.Int("codeintel-repetitions", 3, "number of repeats of every task/configuration")

type configuration struct {
	name                string
	graph, lsp, missing bool
}

var configurations = []configuration{{name: "search_read"}, {name: "graph", graph: true}, {name: "lsp", lsp: true}, {name: "graph_lsp", graph: true, lsp: true}, {name: "graph_missing_lsp", graph: true, lsp: true, missing: true}}

type trace struct {
	Name        string         `json:"name"`
	Operation   string         `json:"operation,omitempty"`
	Args        map[string]any `json:"args"`
	ElapsedMS   int64          `json:"elapsed_ms"`
	OutputBytes int            `json:"output_bytes"`
	Output      string         `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
}
type trial struct {
	Task          string         `json:"task"`
	Configuration string         `json:"configuration"`
	Repeat        int            `json:"repeat"`
	Correct       bool           `json:"correct"`
	Failure       string         `json:"failure,omitempty"`
	ElapsedMS     int64          `json:"elapsed_ms"`
	InputTokens   int64          `json:"input_tokens"`
	OutputTokens  int64          `json:"output_tokens"`
	ToolCalls     int            `json:"tool_calls"`
	ToolErrors    int            `json:"tool_errors"`
	Tools         map[string]int `json:"tools"`
	Operations    map[string]int `json:"operations"`
	Final         string         `json:"final"`
	Trace         []trace        `json:"trace"`
}
type report struct {
	Model          string            `json:"model"`
	Effort         string            `json:"effort"`
	Started        string            `json:"started"`
	Repetitions    int               `json:"repetitions"`
	Build          map[string]string `json:"build"`
	FixtureDigests map[string]string `json:"fixture_sha256"`
	PromptDigest   string            `json:"prompt_sha256"`
	Methodology    []string          `json:"methodology"`
	Trials         []trial           `json:"trials"`
}

// This is a tool-availability ablation on a fixed implementation and prompt,
// not a comparison of revisions. No task directs the model to a specific tool.
func TestModelToolMatrix(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("WINGMAN_LSP_EVAL_MODEL"))
	if model == "" {
		t.Skip("set WINGMAN_LSP_EVAL_MODEL and configure an inference provider")
	}
	if *repetitions < 1 {
		t.Fatal("codeintel-repetitions must be positive")
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	managed, err := devtools.New()
	if err != nil {
		t.Fatal(err)
	}
	if managed.Resolve("gopls") == "" {
		t.Fatal("installed managed gopls required; evaluation never installs tools")
	}
	results := report{Model: model, Effort: "low", Started: time.Now().UTC().Format(time.RFC3339), Repetitions: *repetitions, Methodology: []string{
		"Identical tasks in five tool configurations; search/read/glob always available, edit available only for the rename task.",
		"Configurations: neither analysis tool, code_graph only, LSP only, both, and both advertised with no managed LSP server.",
		"Fixed model instructions and task text. Fixture paths are randomized and do not reveal the configuration. No forced tool choice or availability hint.",
		"Each trial has a fresh workspace, graph cache, language service and model conversation. No cross-trial messages or indexes. Configuration order rotates by task and repeat.",
		"Go parser/type-checker independently verifies reference and call-path ground truth. Rename scoring compares lexical tokens, preserving comment/string content while allowing formatting.",
		"Input/output tokens are provider-reported cumulative totals. Elapsed time includes inference and tool execution, excludes fixture setup. Trials may run concurrently according to go test -parallel; local caches and provider latency are uncontrolled.",
		"Record availability and actual tool usage separately. Graph can use the language service's LSP resolver internally when both are enabled; those RPCs are not separate model tool calls.",
		"All attempts, failures and tool errors are retained. Synthetic Go tasks and one configured model do not establish general language/model performance.",
	}}
	results.Build = map[string]string{"go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range build.Deps {
			results.Build[dependency.Path] = dependency.Version
		}
	}
	results.FixtureDigests = map[string]string{}
	for _, f := range fixtures() {
		data, err := json.Marshal(struct {
			Files   map[string]string
			Request string
		}{f.files, f.request})
		if err != nil {
			t.Fatal(err)
		}
		results.FixtureDigests[f.name] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	results.PromptDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(prompt.BuildBaseInstructions(prompt.VariantFor(model).Agent, prompt.SectionData{}))))
	var mu sync.Mutex
	save := func(result trial) {
		mu.Lock()
		defer mu.Unlock()
		results.Trials = append(results.Trials, result)
		slices.SortFunc(results.Trials, func(a, b trial) int {
			return strings.Compare(fmt.Sprintf("%03d/%s/%s", a.Repeat, a.Task, a.Configuration), fmt.Sprintf("%03d/%s/%s", b.Repeat, b.Task, b.Configuration))
		})
		if *reportPath != "" {
			data, err := json.MarshalIndent(results, "", "  ")
			if err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(*reportPath, append(data, '\n'), 0600); err != nil {
				t.Error(err)
			}
		}
	}
	for repeat := 1; repeat <= *repetitions; repeat++ {
		for taskIndex, f := range fixtures() {
			t.Run(fmt.Sprintf("repeat_%d/%s", repeat, f.name), func(t *testing.T) {
				for step := range len(configurations) {
					cfg := configurations[(step+repeat-1+taskIndex)%len(configurations)]
					t.Run(cfg.name, func(t *testing.T) {
						t.Parallel()
						root := writeFixture(t, f)
						var options []lsp.ManagerOption
						if cfg.lsp && !cfg.missing {
							options = append(options, lsp.WithManagedTools(managed))
						}
						service := language.New(root, filepath.Join(t.TempDir(), "graph.json"), options...)
						defer service.Close()
						fsRoot, err := os.OpenRoot(root)
						if err != nil {
							t.Fatal(err)
						}
						defer fsRoot.Close()
						freshness := fs.NewFreshness(fsRoot)
						var available []tool.Tool
						for _, candidate := range fs.Tools(fsRoot, &fs.Options{Freshness: freshness}) {
							if slices.Contains([]string{"read", "grep", "glob"}, candidate.Name) || f.after != nil && candidate.Name == "edit" {
								available = append(available, candidate)
							}
						}
						if cfg.graph {
							available = append(available, graph.NewTools(service.Graph())...)
						}
						if cfg.lsp {
							available = append(available, freshness.TrackSnapshots(lsptool.NewTools(service)[0]))
						}
						var traceMu sync.Mutex
						var traces []trace
						for i, candidate := range available {
							execute := candidate.Execute
							available[i].Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
								started := time.Now()
								out, err := execute(ctx, args)
								operation, _ := args["operation"].(string)
								event := trace{Name: candidate.Name, Operation: operation, Args: args, ElapsedMS: time.Since(started).Milliseconds(), OutputBytes: len(out.Content), Output: out.Content}
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
						slices.SortFunc(available, func(a, b tool.Tool) int { return strings.Compare(a.Name, b.Name) })
						hcfg, err := harness.ConfigFromEnvironment()
						if err != nil {
							t.Fatal(err)
						}
						hcfg.Model = func() string { return model }
						hcfg.Effort = func() string { return "low" }
						hcfg.MaxTurns = 18
						hcfg.ContextWindow = -1
						hcfg.Tools = func() []tool.Tool { return available }
						hcfg.Instructions = func() string {
							return prompt.BuildBaseInstructions(prompt.VariantFor(model).Agent, prompt.SectionData{}) + "\nWorkspace: " + root + "\nUse the available tools to complete the task. Do not delegate or request new tools. Return the requested answer format."
						}
						a := &harness.Agent{Config: hcfg}
						ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
						defer cancel()
						start := time.Now()
						stream, runErr := a.Send(ctx, []harness.Content{{Text: f.request}})
						if runErr == nil {
							for _, err := range stream {
								if err != nil {
									runErr = err
									break
								}
							}
						}
						result := trial{Task: f.name, Configuration: cfg.name, Repeat: repeat, ElapsedMS: time.Since(start).Milliseconds(), Tools: map[string]int{}, Operations: map[string]int{}}
						for _, message := range a.MessagesSnapshot() {
							var text strings.Builder
							for _, content := range message.Content {
								if call := content.ToolCall; call != nil {
									result.ToolCalls++
									result.Tools[call.Name]++
									var args map[string]any
									if json.Unmarshal([]byte(call.Args), &args) == nil {
										if operation, ok := args["operation"].(string); ok {
											result.Operations[call.Name+"."+operation]++
										}
									}
								}
								if content.ToolResult != nil && content.ToolResult.IsError {
									result.ToolErrors++
								}
								if message.Role == harness.RoleAssistant {
									text.WriteString(content.Text)
								}
							}
							if text.Len() > 0 {
								result.Final = text.String()
							}
						}
						usage := a.UsageSnapshot()
						result.InputTokens = usage.InputTokens
						result.OutputTokens = usage.OutputTokens
						traceMu.Lock()
						result.Trace = slices.Clone(traces)
						traceMu.Unlock()
						result.Correct, result.Failure = score(f, root, result.Final)
						if runErr != nil {
							result.Correct = false
							result.Failure = runErr.Error()
						}
						save(result)
						t.Logf("MATRIX task=%s config=%s repeat=%d correct=%t calls=%d errors=%d input=%d output=%d elapsed_ms=%d tools=%v", f.name, cfg.name, repeat, result.Correct, result.ToolCalls, result.ToolErrors, result.InputTokens, result.OutputTokens, result.ElapsedMS, result.Tools)
						if !result.Correct {
							t.Errorf("%s", result.Failure)
						}
					})
				}
			})
		}
	}
}

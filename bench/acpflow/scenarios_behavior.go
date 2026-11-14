package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"golang.org/x/image/draw"
)

// Prompts never name a tool or a mode rule; checks look only at the resulting workspace and answer.

type scenarioStep struct {
	name string
	// Empty keeps the current session mode.
	mode       string
	unattended bool
	prompt     string
	check      func(ctx context.Context, step stepState) error
}

type stepState struct {
	root       string
	finalText  string
	treeBefore string
}

type scriptedScenario struct {
	fixture func(root string) error
	steps   []scenarioStep
}

var scriptedScenarios = map[string]scriptedScenario{
	"file_ops":     {fixture: writeFileOpsFixture, steps: []scenarioStep{{name: "reorganize", prompt: fileOpsPrompt, check: checkFileOps}}},
	"agent_fanout": {fixture: writeAgentFanoutFixture, steps: []scenarioStep{{name: "fan_out_and_join", prompt: agentFanoutPrompt, check: checkAgentFanout}}},
	"image_read":   {fixture: writeImageReadFixture, steps: []scenarioStep{{name: "read_image", prompt: imageReadPrompt, check: checkImageRead}}},
	"fork_context": {fixture: writeForkContextFixture, steps: []scenarioStep{{name: "handoff", prompt: forkContextPrompt, check: checkForkContext}}},
	"unattended":   {fixture: writeUnattendedFixture, steps: []scenarioStep{{name: "ambiguous_request", unattended: true, prompt: unattendedPrompt, check: checkUnattended}}},
	"slash_review": {fixture: writeBugReviewFixture, steps: []scenarioStep{{name: "review", prompt: "/review", check: func(_ context.Context, step stepState) error {
		return checkBugReviewFindings(step.root, step.finalText)
	}}}},
	"mode_switch": {fixture: writeModeSwitchFixture, steps: []scenarioStep{
		{name: "plan_imperative", mode: "plan", prompt: modeSwitchPlanPrompt, check: checkPlanOnly},
		{name: "implement", mode: "agent", prompt: modeSwitchBuildPrompt, check: checkModeSwitchBuild},
		{name: "plan_again", mode: "plan", prompt: modeSwitchFollowUpPrompt, check: checkPlanOnly},
	}},
}

func runScriptedScenario(ctx context.Context, conn *acp.ClientSideConnection, client *benchClient, recorder *requestRecorder, session acp.NewSessionResponse, root string, script scriptedScenario) []benchTurn {
	var turns []benchTurn
	for _, step := range script.steps {
		turn := benchTurn{Name: step.name}
		if err := selectSessionMode(ctx, conn, session, step.mode); err != nil {
			turn.Error = "mode: " + err.Error()
			return append(turns, turn)
		}
		if step.unattended {
			if _, err := conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: session.SessionId, ConfigId: "unattended", Value: "on",
			}}); err != nil {
				turn.Error = "unattended: " + err.Error()
				return append(turns, turn)
			}
		}
		state := stepState{root: root, treeBefore: treeDigest(root)}
		firstExchange := recorder.count()
		started := time.Now()
		client.reset(started)
		response, err := conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock(step.prompt)},
		})
		turn.TotalMS = time.Since(started).Milliseconds()
		turn.Events, turn.FinalText = client.snapshot()
		exchanges := recorder.snapshot()
		summary := analyzeBehavior(exchanges[firstExchange:], leadConversation(exchanges))
		turn.Behavior = &summary
		if err != nil {
			turn.Error = "prompt: " + err.Error()
			return append(turns, turn)
		}
		turn.StopReason = string(response.StopReason)
		turn.Usage = response.Usage
		state.finalText = turn.FinalText
		if checkErr := step.check(ctx, state); checkErr != nil {
			turn.Error = "verification: " + checkErr.Error()
		} else {
			turn.BuildPassed, turn.RequirementsPassed = true, true
		}
		turns = append(turns, turn)
	}
	return turns
}

// Agents name their default working mode differently.
func selectSessionMode(ctx context.Context, conn *acp.ClientSideConnection, session acp.NewSessionResponse, want string) error {
	if want == "" {
		return nil
	}
	if session.Modes == nil {
		return fmt.Errorf("agent advertises no session modes")
	}
	candidates := []string{want}
	if want == "agent" {
		candidates = append(candidates, "code", "default", "auto")
	}
	for _, candidate := range candidates {
		for _, mode := range session.Modes.AvailableModes {
			if string(mode.Id) != candidate {
				continue
			}
			_, err := conn.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: session.SessionId, ModeId: mode.Id})
			return err
		}
	}
	return fmt.Errorf("agent does not advertise mode %q", want)
}

// Ignores Git state and build caches, so equal digests mean the files were left alone.
func treeDigest(root string) string {
	hash := sha256.New()
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == ".gocache" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		hash.Write(data)
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil))
}

func exists(root string, parts ...string) bool {
	_, err := os.Stat(filepath.Join(append([]string{root}, parts...)...))
	return err == nil
}

const fileOpsPrompt = `Reorganize this repository without changing behavior:
1. Rename the package internal/util to internal/textutil (directory and package name) and update every import and reference.
2. Rename cmd/shop/main_helpers.go to cmd/shop/format.go.
3. Remove the unused internal/legacy package.
Verify with go build ./... and go test ./...`

func writeFileOpsFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.26\n",
		"internal/util/strings.go": `package util

import "strings"

// Title upper-cases the first letter of every word.
func Title(s string) string {
	words := strings.Fields(s)
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
	}
	return strings.Join(words, " ")
}

// Truncate shortens s to at most n runes, marking the cut with an ellipsis.
func Truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}
`,
		"internal/util/strings_test.go": `package util

import "testing"

func TestTitle(t *testing.T) {
	if got := Title("blue  ceramic MUG"); got != "Blue Ceramic Mug" {
		t.Fatalf("Title() = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("ceramic", 4); got != "cer…" {
		t.Fatalf("Truncate() = %q", got)
	}
	if got := Truncate("mug", 4); got != "mug" {
		t.Fatalf("Truncate() = %q", got)
	}
}
`,
		"internal/util/slug.go": `package util

import (
	"strings"
	"unicode"
)

// Slug converts a title to a lowercase, dash-separated identifier.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}
`,
		"internal/util/slug_test.go": `package util

import "testing"

func TestSlug(t *testing.T) {
	if got := Slug("  Blue Ceramic Mug (350ml) "); got != "blue-ceramic-mug-350ml" {
		t.Fatalf("Slug() = %q", got)
	}
}
`,
		"internal/legacy/format.go": `package legacy

import "fmt"

// FormatV1 renders the price format used before prices moved to cents.
func FormatV1(dollars float64) string {
	return fmt.Sprintf("$%.2f", dollars)
}
`,
		"internal/legacy/format_test.go": `package legacy

import "testing"

func TestFormatV1(t *testing.T) {
	if got := FormatV1(3.5); got != "$3.50" {
		t.Fatalf("FormatV1() = %q", got)
	}
}
`,
		"catalog/product.go": `package catalog

import "example.com/shop/internal/util"

type Product struct {
	Name       string
	PriceCents int
}

// Handle is the URL-safe identifier of a product.
func (p Product) Handle() string {
	return util.Slug(p.Name)
}

// DisplayName is the product name as shown in listings.
func (p Product) DisplayName() string {
	return util.Truncate(util.Title(p.Name), 24)
}
`,
		"catalog/product_test.go": `package catalog

import "testing"

func TestProduct(t *testing.T) {
	p := Product{Name: "blue ceramic mug with an extra long product name", PriceCents: 1250}
	if got := p.Handle(); got != "blue-ceramic-mug-with-an-extra-long-product-name" {
		t.Fatalf("Handle() = %q", got)
	}
	if got := p.DisplayName(); got != "Blue Ceramic Mug With A…" {
		t.Fatalf("DisplayName() = %q", got)
	}
}
`,
		"cmd/shop/main.go": `package main

import (
	"fmt"

	"example.com/shop/catalog"
)

func main() {
	for _, product := range []catalog.Product{
		{Name: "blue ceramic mug", PriceCents: 1250},
		{Name: "walnut serving tray", PriceCents: 4800},
	} {
		fmt.Println(formatLine(product))
	}
}
`,
		"cmd/shop/main_helpers.go": `package main

import (
	"fmt"

	"example.com/shop/catalog"
	"example.com/shop/internal/util"
)

func formatLine(p catalog.Product) string {
	return fmt.Sprintf("%-14s %s  %d.%02d", util.Truncate(p.Handle(), 14), p.DisplayName(), p.PriceCents/100, p.PriceCents%100)
}
`,
	})
}

func checkFileOps(ctx context.Context, step stepState) error {
	for _, name := range []string{"strings.go", "strings_test.go", "slug.go", "slug_test.go"} {
		data, err := os.ReadFile(filepath.Join(step.root, "internal", "textutil", name))
		if err != nil {
			return fmt.Errorf("internal/textutil/%s missing", name)
		}
		if !regexp.MustCompile(`(?m)^package textutil\b`).Match(data) {
			return fmt.Errorf("internal/textutil/%s does not declare package textutil", name)
		}
	}
	for _, gone := range []string{"internal/util", "internal/legacy", "cmd/shop/main_helpers.go"} {
		if exists(step.root, filepath.FromSlash(gone)) {
			return fmt.Errorf("%s still exists", gone)
		}
	}
	if !exists(step.root, "cmd", "shop", "format.go") {
		return fmt.Errorf("cmd/shop/format.go missing")
	}
	var stale string
	_ = filepath.WalkDir(step.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, _ := os.ReadFile(path)
		if bytes.Contains(data, []byte(`internal/util"`)) || bytes.Contains(data, []byte("internal/legacy")) {
			stale, _ = filepath.Rel(step.root, path)
		}
		return nil
	})
	if stale != "" {
		return fmt.Errorf("%s still references a removed package", stale)
	}
	return runGoTests(ctx, step.root, false, "./...")
}

const agentFanoutPrompt = `Map the HTTP API of this repository. Use one background subagent per service directory under services/ (billing, inventory, notify) so the three run in parallel; each reports every HTTP route its service registers, with method and path. Do not read the service code yourself. Once all three have reported, write ROUTES.md at the repository root listing every route grouped by service.`

var agentFanoutRoutes = map[string][]string{
	"billing":   {"GET /v1/invoices", "POST /v1/invoices/{id}/void", "GET /v1/invoices/{id}/pdf", "POST /internal/billing/reconcile"},
	"inventory": {"GET /v1/stock/{sku}", "PUT /v1/stock/{sku}/reserve", "DELETE /v1/stock/{sku}/reserve", "POST /internal/inventory/recount"},
	"notify":    {"POST /v1/notifications", "GET /v1/notifications/{id}/status", "POST /v1/webhooks/bounce", "POST /internal/notify/drain"},
}

func writeAgentFanoutFixture(root string) error {
	files := map[string]string{
		"go.mod":    "module example.com/platform\n\ngo 1.26\n",
		"README.md": "# Platform\n\nThree HTTP services live under `services/`.\n",
	}
	for service, routes := range agentFanoutRoutes {
		// Routes are split across files so a complete answer needs a real look at each service.
		var public, internal strings.Builder
		for i, route := range routes {
			line := fmt.Sprintf("\tmux.HandleFunc(%q, s.handle%d)\n", route, i)
			if strings.Contains(route, "/internal/") {
				internal.WriteString(line)
			} else {
				public.WriteString(line)
			}
		}
		var handlers strings.Builder
		for i := range routes {
			fmt.Fprintf(&handlers, "\nfunc (s *Server) handle%d(w http.ResponseWriter, r *http.Request) {\n\ts.requests++\n\tw.WriteHeader(http.StatusNoContent)\n}\n", i)
		}
		dir := "services/" + service + "/"
		files[dir+"server.go"] = fmt.Sprintf("package %s\n\nimport \"net/http\"\n\n// Server holds the %s service state.\ntype Server struct {\n\trequests int\n}\n\n// Handler returns the service's HTTP handler.\nfunc (s *Server) Handler() http.Handler {\n\tmux := http.NewServeMux()\n\ts.registerPublic(mux)\n\ts.registerInternal(mux)\n\treturn mux\n}\n", service, service)
		files[dir+"routes.go"] = fmt.Sprintf("package %s\n\nimport \"net/http\"\n\nfunc (s *Server) registerPublic(mux *http.ServeMux) {\n%s}\n", service, public.String())
		files[dir+"admin.go"] = fmt.Sprintf("package %s\n\nimport \"net/http\"\n\n// Internal routes are reachable only from the service mesh.\nfunc (s *Server) registerInternal(mux *http.ServeMux) {\n%s}\n", service, internal.String())
		files[dir+"handlers.go"] = fmt.Sprintf("package %s\n\nimport \"net/http\"\n%s", service, handlers.String())
	}
	return initGitRepoWithInitialCommit(root, files)
}

func checkAgentFanout(_ context.Context, step stepState) error {
	data, err := os.ReadFile(filepath.Join(step.root, "ROUTES.md"))
	if err != nil {
		return fmt.Errorf("ROUTES.md was not written")
	}
	var missing []string
	for _, routes := range agentFanoutRoutes {
		for _, route := range routes {
			_, path, _ := strings.Cut(route, " ")
			if !bytes.Contains(data, []byte(path)) {
				missing = append(missing, route)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("ROUTES.md is missing %d of 12 routes, e.g. %s", len(missing), missing[0])
	}
	return nil
}

// Broad colour stripes are read reliably and cannot be guessed.
var imageReadStripes = []struct {
	name  string
	color color.RGBA
}{
	{"green", color.RGBA{0x1e, 0x9e, 0x3e, 0xff}},
	{"red", color.RGBA{0xe0, 0x20, 0x1b, 0xff}},
	{"blue", color.RGBA{0x1f, 0x4f, 0xd8, 0xff}},
	{"yellow", color.RGBA{0xf7, 0xd9, 0x17, 0xff}},
}

const imageReadPrompt = `Open assets/flag.png and list the colors of its vertical stripes from left to right.`

func writeImageReadFixture(root string) error {
	const stripeWidth, height = 120, 240
	flag := image.NewRGBA(image.Rect(0, 0, stripeWidth*len(imageReadStripes), height))
	for i, stripe := range imageReadStripes {
		area := image.Rect(i*stripeWidth, 0, (i+1)*stripeWidth, height)
		draw.Draw(flag, area, image.NewUniform(stripe.color), image.Point{}, draw.Src)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, flag); err != nil {
		return err
	}
	if err := initGitRepoWithInitialCommit(root, map[string]string{
		"README.md": "# Flags\n\nReference flag images are stored under `assets/`.\n",
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "assets", "flag.png"), encoded.Bytes(), 0o644)
}

func checkImageRead(_ context.Context, step stepState) error {
	answer := strings.ToLower(step.finalText)
	position := 0
	for _, stripe := range imageReadStripes {
		index := strings.Index(answer[position:], stripe.name)
		if index < 0 {
			return fmt.Errorf("answer does not list the stripes in order (green, red, blue, yellow): %s", preview(step.finalText))
		}
		position += index + len(stripe.name)
	}
	return nil
}

const forkContextPrompt = `Read docs/storage.md, docs/queue.md, and docs/auth.md. Then hand the write-up to a subagent that inherits what you have already read in this conversation, instead of repeating the documents to it: it should write LIMITS.md with one bullet per document stating that document's hard limit. Tell me when it is done.`

var forkContextFacts = []string{"64 MiB", "17", "45"}

func writeForkContextFixture(root string) error {
	filler := func(topic string) string {
		var b strings.Builder
		for i := 1; i <= 24; i++ {
			fmt.Fprintf(&b, "%d. The %s subsystem records operation %d in the audit trail and exposes it on the metrics endpoint.\n", i, topic, i)
		}
		return b.String()
	}
	return initGitRepoWithInitialCommit(root, map[string]string{
		"docs/storage.md": "# Storage\n\n" + filler("storage") + "\n## Limits\n\nA single object may not exceed 64 MiB; larger uploads are rejected with status 413.\n",
		"docs/queue.md":   "# Queue\n\n" + filler("queue") + "\n## Limits\n\nA message is redelivered at most 17 times before it moves to the dead-letter queue.\n",
		"docs/auth.md":    "# Auth\n\n" + filler("auth") + "\n## Limits\n\nAccess tokens expire after 45 minutes and cannot be extended.\n",
	})
}

func checkForkContext(_ context.Context, step stepState) error {
	data, err := os.ReadFile(filepath.Join(step.root, "LIMITS.md"))
	if err != nil {
		return fmt.Errorf("LIMITS.md was not written")
	}
	for _, fact := range forkContextFacts {
		if !bytes.Contains(data, []byte(fact)) {
			return fmt.Errorf("LIMITS.md does not state the limit %q", fact)
		}
	}
	return nil
}

const unattendedPrompt = `Add a --format flag to cmd/report that selects the output format. Support the formats you think make sense; if anything is unclear, ask me. Keep the current output as the default and make sure go test ./... passes.`

func writeUnattendedFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/report\n\ngo 1.26\n",
		"cmd/report/main.go": `package main

import (
	"fmt"
	"io"
	"os"
)

type row struct {
	Name  string
	Count int
}

var rows = []row{{"builds", 42}, {"deploys", 7}, {"rollbacks", 1}}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string, out io.Writer) error {
	for _, r := range rows {
		fmt.Fprintf(out, "%-10s %d\n", r.Name, r.Count)
	}
	return nil
}
`,
		"cmd/report/main_test.go": `package main

import (
	"bytes"
	"testing"
)

func TestDefaultOutput(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatal(err)
	}
	if want := "builds     42\ndeploys    7\nrollbacks  1\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}
`,
	})
}

func checkUnattended(ctx context.Context, step stepState) error {
	data, err := os.ReadFile(filepath.Join(step.root, "cmd", "report", "main.go"))
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte("format")) {
		return fmt.Errorf("cmd/report/main.go has no format flag")
	}
	return runGoTests(ctx, step.root, false, "./...")
}

// Imperative on purpose: only the session mode says the first and third turns must not change files.
const modeSwitchPlanPrompt = `RunWithRetry in taskqueue is broken: it keeps sleeping after the final failed attempt, ignores context cancellation while it waits, and never caps the delay at MaxDelay. Fix it and run the tests.`

const modeSwitchBuildPrompt = `Go ahead.`

const modeSwitchFollowUpPrompt = `Also make BaseDelay default to 10ms when it is zero, and cover that with a test.`

func writeModeSwitchFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/taskqueue\n\ngo 1.26\n",
		"taskqueue/runner.go": `package taskqueue

import (
	"context"
	"time"
)

type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// RunWithRetry executes fn up to MaxAttempts times. Between attempts it waits
// BaseDelay, doubling each time up to MaxDelay, and stops early with the
// context's error if ctx is canceled while waiting.
func RunWithRetry(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	delay := cfg.BaseDelay
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := fn(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(delay)
		delay *= 2
	}
	return lastErr
}
`,
		"taskqueue/runner_test.go": `package taskqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunWithRetryCancelsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := RunWithRetry(ctx, Config{MaxAttempts: 5, BaseDelay: 500 * time.Millisecond, MaxDelay: 600 * time.Millisecond}, func(context.Context) error {
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("backoff ignored context cancellation; took %v", elapsed)
	}
}

func TestRunWithRetryDoesNotSleepAfterFinalAttempt(t *testing.T) {
	start := time.Now()
	sentinel := errors.New("boom")
	err := RunWithRetry(context.Background(), Config{MaxAttempts: 2, BaseDelay: 20 * time.Millisecond, MaxDelay: 25 * time.Millisecond}, func(context.Context) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 40*time.Millisecond {
		t.Fatalf("slept after final attempt; took %v", elapsed)
	}
}

func TestRunWithRetryCapsDelay(t *testing.T) {
	start := time.Now()
	_ = RunWithRetry(context.Background(), Config{MaxAttempts: 4, BaseDelay: 20 * time.Millisecond, MaxDelay: 30 * time.Millisecond}, func(context.Context) error {
		return errors.New("transient")
	})
	// 20ms + 30ms + 30ms; an uncapped doubling would wait 20 + 40 + 80.
	if elapsed := time.Since(start); elapsed >= 120*time.Millisecond {
		t.Fatalf("delay was not capped at MaxDelay; took %v", elapsed)
	}
}
`,
	})
}

func checkPlanOnly(_ context.Context, step stepState) error {
	if treeDigest(step.root) != step.treeBefore {
		return fmt.Errorf("files changed during a Plan-mode turn")
	}
	if len(strings.TrimSpace(step.finalText)) < 150 {
		return fmt.Errorf("no plan was presented: %s", preview(step.finalText))
	}
	return nil
}

func checkModeSwitchBuild(ctx context.Context, step stepState) error {
	if treeDigest(step.root) == step.treeBefore {
		return fmt.Errorf("nothing was implemented after leaving Plan mode")
	}
	return runGoTests(ctx, step.root, true, "./...")
}

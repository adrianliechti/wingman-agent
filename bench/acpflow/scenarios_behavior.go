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
	"maps"
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
	"move_asset":   {fixture: writeMoveAssetFixture, steps: []scenarioStep{{name: "move_asset", prompt: "Move assets/release.bin to dist/release.bin and preserve its contents.", check: checkMoveAsset}}},
	"file_ops":     {fixture: writeFileOpsFixture, steps: []scenarioStep{{name: "reorganize", prompt: fileOpsPrompt, check: checkFileOps}}},
	"agent_fanout": {fixture: writeAgentFanoutFixture, steps: []scenarioStep{{name: "fan_out_and_join", prompt: agentFanoutPrompt, check: checkAgentFanout}}},
	"image_read":   {fixture: writeImageReadFixture, steps: []scenarioStep{{name: "read_image", prompt: imageReadPrompt, check: checkImageRead}}},
	"fork_context": {fixture: writeForkContextFixture, steps: []scenarioStep{{name: "handoff", prompt: forkContextPrompt, check: checkForkContext}}},
	"unattended":   {fixture: writeUnattendedFixture, steps: []scenarioStep{{name: "ambiguous_request", unattended: true, prompt: unattendedPrompt, check: checkUnattended}}},
	"slash_review": {fixture: writeBugReviewFixture, steps: []scenarioStep{{name: "review", prompt: "/review", check: func(_ context.Context, step stepState) error {
		return checkBugReviewFindings(step.root, step.finalText)
	}}}},
	// Requests an agent can either carry out or hand back as a proposal; question_only is the counterpart that must stay an answer.
	"soft_request":   {fixture: writeUnattendedFixture, steps: []scenarioStep{{name: "soft_request", prompt: softRequestPrompt, check: checkSoftRequest}}},
	"open_choice":    {fixture: writeUnattendedFixture, steps: []scenarioStep{{name: "open_choice", prompt: unattendedPrompt, check: checkUnattended}}},
	"plan_then_do":   {fixture: writeModeSwitchFixture, steps: []scenarioStep{{name: "plan_then_do", prompt: planThenDoPrompt, check: checkModeSwitchBuild}}},
	"multi_part":     {fixture: writeFileOpsFixture, steps: []scenarioStep{{name: "multi_part", prompt: multiPartPrompt, check: checkMultiPart}}},
	"question_only":  {fixture: writeModeSwitchFixture, steps: []scenarioStep{{name: "question_only", prompt: questionOnlyPrompt, check: checkQuestionOnly}}},
	"problem_report": {fixture: writeModeSwitchFixture, steps: []scenarioStep{{name: "problem_report", prompt: problemReportPrompt, check: checkModeSwitchBuild}}},
	"adopt_refs":     {fixture: writeAdoptRefsFixture, steps: []scenarioStep{{name: "adopt_refs", prompt: adoptRefsPrompt, check: checkAdoptRefs}}},
	"adopt_sibling":  {fixture: writeAdoptSiblingFixture, steps: []scenarioStep{{name: "adopt_sibling", prompt: adoptSiblingPrompt, check: checkAdoptRefs}}},
	"edit_sibling":   {fixture: writeAdoptSiblingFixture, steps: []scenarioStep{{name: "edit_sibling", prompt: editSiblingPrompt, check: checkEditSibling}}},
	"migrate_all":    {fixture: writeMigrateAllFixture, steps: []scenarioStep{{name: "migrate_all", prompt: migrateAllPrompt, check: checkMigrateAll}}},
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
			Prompt:    []acp.ContentBlock{acp.TextBlock(scenarioPrompt(step.prompt, root))},
		})
		turn.TotalMS = time.Since(started).Milliseconds()
		turn.Events, turn.FinalText = client.snapshot()
		exchanges := recorder.snapshot()
		summary := analyzeBehavior(exchanges[firstExchange:], leadConversation(exchanges))
		if asksBack(turn.FinalText) {
			summary.Signals["asks_back"]++
		}
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

const moveAssetPattern = "\x00\xffrelease-v1"

func writeMoveAssetFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{"assets/release.bin": strings.Repeat(moveAssetPattern, 1<<20)})
}

func checkMoveAsset(_ context.Context, step stepState) error {
	if _, err := os.Lstat(filepath.Join(step.root, "assets", "release.bin")); !os.IsNotExist(err) {
		return fmt.Errorf("source still present or inaccessible: %v", err)
	}
	dst := filepath.Join(step.root, "dist", "release.bin")
	info, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(moveAssetPattern))<<20 {
		return fmt.Errorf("destination has wrong type or size")
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	if len(data) != len(moveAssetPattern)*(1<<20) || bytes.Count(data, []byte(moveAssetPattern)) != 1<<20 {
		return fmt.Errorf("renamed file differs")
	}
	return nil
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
		// A second package ending in "util" makes a careless text substitution break the build.
		"internal/timeutil/stamp.go": `package timeutil

import "time"

// Stamp formats t as a compact UTC timestamp.
func Stamp(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}
`,
		"catalog/product.go": `package catalog

import (
	"time"

	"example.com/shop/internal/timeutil"
	"example.com/shop/internal/util"
)

type Product struct {
	Name       string
	PriceCents int
	AddedAt    time.Time
}

// Added is the listing timestamp of a product.
func (p Product) Added() string {
	return timeutil.Stamp(p.AddedAt)
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
	"example.com/shop/internal/timeutil"
	"example.com/shop/internal/util"
)

func formatLine(p catalog.Product) string {
	return fmt.Sprintf("%-14s %s  %d.%02d  %s", util.Truncate(p.Handle(), 14), p.DisplayName(), p.PriceCents/100, p.PriceCents%100, timeutil.Stamp(p.AddedAt))
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
	for _, kept := range []string{"cmd/shop/format.go", "internal/timeutil/stamp.go"} {
		if !exists(step.root, filepath.FromSlash(kept)) {
			return fmt.Errorf("%s missing", kept)
		}
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

const softRequestPrompt = `let's give cmd/report a --format flag so it can also print json. the current output should stay the default. also check whether the tests still cover the default output`

func checkSoftRequest(ctx context.Context, step stepState) error {
	data, err := os.ReadFile(filepath.Join(step.root, "cmd", "report", "main.go"))
	if err != nil {
		return err
	}
	for _, want := range []string{"format", "json"} {
		if !bytes.Contains(bytes.ToLower(data), []byte(want)) {
			return fmt.Errorf("cmd/report/main.go does not mention %s", want)
		}
	}
	return runGoTests(ctx, step.root, false, "./...")
}

const planThenDoPrompt = `RunWithRetry in taskqueue has three problems: it keeps sleeping after the final failed attempt, ignores context cancellation while it waits, and never caps the delay at MaxDelay. Come up with a plan first, then fix it.`

const multiPartPrompt = `A few things in this repo have been bugging me: Title mangles words that start with a non-ASCII letter (try "über mug"), Truncate can cut right before a space and leave a dangling one in front of the ellipsis, and I don't think anything uses internal/legacy anymore. Can you sort these out?`

const multiPartProbe = `package util

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBenchProbe(t *testing.T) {
	if got := Title("über mug"); got != "Über Mug" {
		t.Fatalf("Title = %q", got)
	}
	got := Truncate("blue mug set", 6)
	if strings.Contains(got, " …") || !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) > 6 {
		t.Fatalf("Truncate = %q", got)
	}
}
`

func checkMultiPart(ctx context.Context, step stepState) error {
	// An emptied directory counts: Git does not track it.
	if left, _ := filepath.Glob(filepath.Join(step.root, "internal", "legacy", "*.go")); len(left) > 0 {
		return fmt.Errorf("internal/legacy still has Go files")
	}
	probe := filepath.Join(step.root, "internal", "util", "zz_bench_probe_test.go")
	if err := os.WriteFile(probe, []byte(multiPartProbe), 0o644); err != nil {
		return err
	}
	defer os.Remove(probe)
	return runGoTests(ctx, step.root, false, "./...")
}

const adoptRefsPrompt = `let's adopt the retry behaviour from third_party/alpha and third_party/beta in our fetch client; also check docs/notes.md if there are limits we should respect`

// The same request with the references in a directory next to the workspace.
const adoptSiblingPrompt = `let's adopt the retry behaviour from ` + siblingRefs + `/alpha and ` + siblingRefs + `/beta in our fetch client; also check ` + siblingRefs + `/notes.md if there are limits we should respect`

// A fixture may keep material beside its workspace in <workspace>-refs; prompts name it through siblingRefs.
const (
	siblingRefs   = "../{{workspace}}" + siblingSuffix
	siblingSuffix = "-refs"
)

func scenarioPrompt(prompt, root string) string {
	return strings.ReplaceAll(prompt, "{{workspace}}", filepath.Base(root))
}

const adoptClientSource = `package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client downloads documents from the catalog service.
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// Get fetches path and returns the response body.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		body, err := c.get(ctx, path)
		if err == nil {
			return body, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, lastErr
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
`

var adoptWorkspaceFiles = map[string]string{
	"go.mod":          "module example.com/fetcher\n\ngo 1.26\n",
	"fetch/client.go": adoptClientSource,
	"fetch/client_test.go": `package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetReturnsBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), BaseURL: server.URL}
	body, err := client.Get(context.Background(), "/doc")
	if err != nil || string(body) != "ok" {
		t.Fatalf("Get() = %q, %v", body, err)
	}
}
`,
}

var adoptReferenceFiles = map[string]string{
	"alpha/retry.go": `// Package alpha is the retry helper used by the ingest service.
package alpha

import (
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

const (
	BaseDelay = 200 * time.Millisecond
	MaxDelay  = 5 * time.Second
)

// Retryable reports whether a response status is worth another attempt.
func Retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// Backoff returns the wait before the given retry (1-based): exponential
// growth from BaseDelay, capped at MaxDelay, with full jitter.
func Backoff(retry int) time.Duration {
	delay := BaseDelay << (retry - 1)
	if delay > MaxDelay || delay <= 0 {
		delay = MaxDelay
	}
	return time.Duration(rand.Int63n(int64(delay) + 1))
}

// RetryAfter returns the server-requested wait from a Retry-After header in
// seconds, or zero when it is absent or malformed.
func RetryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
`,
	"beta/policy.go": `// Package beta is the retry policy used by the billing exporter.
package beta

import (
	"context"
	"time"
)

type Policy struct {
	MaxAttempts    int
	AttemptTimeout time.Duration
	Budget         time.Duration
}

var Default = Policy{MaxAttempts: 5, AttemptTimeout: 10 * time.Second, Budget: 30 * time.Second}

// Sleep waits for d or until ctx ends, whichever comes first.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
`,
	"notes.md": `# Catalog service notes

- The catalog rate limiter resets within 8 seconds, so there is no point in backing off longer than that.
- Operations asked us to keep it to 4 attempts per request at most.
- A 404 means the document is gone. Retrying it only adds load.
`,
}

func writeAdoptRefsFixture(root string) error {
	files := maps.Clone(adoptWorkspaceFiles)
	for name, content := range adoptReferenceFiles {
		if name == "notes.md" {
			files["docs/notes.md"] = content
			continue
		}
		files["third_party/"+name] = content
	}
	return initGitRepoWithInitialCommit(root, files)
}

func writeAdoptSiblingFixture(root string) error {
	refs := root + siblingSuffix
	for name, content := range adoptReferenceFiles {
		path := filepath.Join(refs, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(refs, "go.mod"), []byte("module example.com/references\n\ngo 1.26\n"), 0o644); err != nil {
		return err
	}
	return initGitRepoWithInitialCommit(root, adoptWorkspaceFiles)
}

// Run the client's actual retry loop under virtual time. The transport records
// each request without opening sockets or waiting through real backoffs.
const fetchProbe = `package fetch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type benchTransport func(*http.Request) (*http.Response, error)

func (f benchTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func probeRetry(t *testing.T, statuses []int, retryAfter string, minCalls, maxCalls int, wantSuccess bool, minWait, maxWait time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var calls int
		var first, second time.Time
		transport := benchTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				first = time.Now()
			}
			if calls == 2 {
				second = time.Now()
			}
			if calls > maxCalls {
				t.Fatalf("unexpected request %d (want %d)", calls, maxCalls)
			}
			status := statuses[min(calls, len(statuses))-1]
			return &http.Response{
				StatusCode: status, Header: http.Header{"Retry-After": []string{retryAfter}},
				Body: io.NopCloser(strings.NewReader("body")), Request: req,
			}, nil
		})
		client := &Client{BaseURL: "http://catalog.test", HTTP: &http.Client{Transport: transport}}
		body, err := client.Get(context.Background(), "/doc")
		if (calls < minCalls || calls > maxCalls) || (err == nil) != wantSuccess || (wantSuccess && string(body) != "body") {
			t.Fatalf("Get() after %d attempts = %q, %v; want %d..%d attempts, success=%t", calls, body, err, minCalls, maxCalls, wantSuccess)
		}
		if maxWait > 0 && (second.Sub(first) < minWait || second.Sub(first) > maxWait) {
			t.Fatalf("first retry waited %s, want %s to %s", second.Sub(first), minWait, maxWait)
		}
	})
}
`

const adoptProbe = fetchProbe + `
func TestBenchRetryPolicy(t *testing.T) {
	t.Run("gone", func(t *testing.T) { probeRetry(t, []int{404}, "", 1, 1, false, 0, 0) })
	t.Run("transient", func(t *testing.T) { probeRetry(t, []int{503, 200}, "", 2, 2, true, 0, 0) })
	t.Run("rate limited", func(t *testing.T) { probeRetry(t, []int{429, 200}, "2", 2, 2, true, 2*time.Second, 8*time.Second) })
	t.Run("delay cap", func(t *testing.T) { probeRetry(t, []int{429, 200}, "60", 2, 2, true, 0, 8*time.Second) })
	t.Run("attempt limit", func(t *testing.T) { probeRetry(t, []int{503}, "", 2, 4, false, 0, 0) })
}
`

const editSiblingProbe = fetchProbe + `
func TestBenchAttempts(t *testing.T) { probeRetry(t, []int{503}, "", 4, 4, false, 0, 0) }
`

// The verifier runs after the agent returns. A unique file preserves any tests
// the agent added and disappears even when verification fails.
func runProbe(ctx context.Context, root, pkg, source string) error {
	f, err := os.CreateTemp(filepath.Join(root, pkg), "zz_bench_*_test.go")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(source)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return runGoTests(ctx, root, false, "./...")
}

func checkAdoptRefs(ctx context.Context, step stepState) error {
	return runProbe(ctx, step.root, "fetch", adoptProbe)
}

const editSiblingPrompt = `ops wants 4 attempts at most. our fetch client retries 3 times, make that 4, and set MaxAttempts to 4 in ` + siblingRefs + `/beta/policy.go as well so the two stay in sync`

func checkEditSibling(ctx context.Context, step stepState) error {
	const policyProbe = `package beta

import "testing"

func TestBenchDefault(t *testing.T) {
	if got := Default.MaxAttempts; got != 4 {
		t.Fatalf("MaxAttempts = %d, want 4", got)
	}
}
`
	if err := runProbe(ctx, step.root+siblingSuffix, "beta", policyProbe); err != nil {
		return err
	}
	return runProbe(ctx, step.root, "fetch", editSiblingProbe)
}

const migrateAllPrompt = `let's move everything off legacylog to log/slog, it has been deprecated forever`

func writeMigrateAllFixture(root string) error {
	files := map[string]string{
		"go.mod": "module example.com/ops\n\ngo 1.26\n",
		"legacylog/legacylog.go": `// Package legacylog is the logging helper from before the standard library had one.
//
// Deprecated: use log/slog.
package legacylog

import (
	"fmt"
	"os"
)

func Printf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "INFO "+format+"\n", args...)
}

func Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR "+format+"\n", args...)
}
`,
		"billing/invoice.go": `package billing

import "example.com/ops/legacylog"

// Total sums line amounts in cents.
func Total(lines []int) int {
	total := 0
	for _, line := range lines {
		total += line
	}
	legacylog.Printf("invoice total=%d lines=%d", total, len(lines))
	return total
}
`,
		"billing/invoice_test.go": `package billing

import "testing"

func TestTotal(t *testing.T) {
	if got := Total([]int{250, 125}); got != 375 {
		t.Fatalf("Total() = %d", got)
	}
}
`,
		"billing/refund.go": `package billing

import (
	"errors"

	"example.com/ops/legacylog"
)

// Refund returns the refundable amount, which cannot exceed what was paid.
func Refund(paid, requested int) (int, error) {
	if requested > paid {
		legacylog.Errorf("refund rejected paid=%d requested=%d", paid, requested)
		return 0, errors.New("refund exceeds payment")
	}
	legacylog.Printf("refund accepted amount=%d", requested)
	return requested, nil
}
`,
		"inventory/stock.go": `package inventory

import "example.com/ops/legacylog"

// Reserve takes quantity out of stock and reports whether it was available.
func Reserve(stock map[string]int, sku string, quantity int) bool {
	if stock[sku] < quantity {
		legacylog.Errorf("out of stock sku=%s wanted=%d have=%d", sku, quantity, stock[sku])
		return false
	}
	stock[sku] -= quantity
	legacylog.Printf("reserved sku=%s quantity=%d", sku, quantity)
	return true
}
`,
		"inventory/stock_test.go": `package inventory

import "testing"

func TestReserve(t *testing.T) {
	stock := map[string]int{"mug": 2}
	if !Reserve(stock, "mug", 2) || Reserve(stock, "mug", 1) {
		t.Fatalf("Reserve() left %v", stock)
	}
}
`,
		"inventory/reorder.go": `package inventory

import "example.com/ops/legacylog"

// Reorder lists the SKUs at or below their threshold.
func Reorder(stock map[string]int, threshold int) []string {
	var low []string
	for sku, quantity := range stock {
		if quantity <= threshold {
			low = append(low, sku)
		}
	}
	legacylog.Printf("reorder candidates=%d threshold=%d", len(low), threshold)
	return low
}
`,
		"shipping/label.go": `package shipping

import (
	"fmt"

	"example.com/ops/legacylog"
)

// Label formats the address block printed on a parcel.
func Label(name, street, city string) string {
	if name == "" {
		legacylog.Errorf("label without recipient street=%q city=%q", street, city)
	}
	return fmt.Sprintf("%s\n%s\n%s", name, street, city)
}
`,
		"shipping/track.go": `package shipping

import "example.com/ops/legacylog"

// Status maps a carrier code to the status shown to customers.
func Status(code string) string {
	switch code {
	case "DL":
		return "delivered"
	case "TR":
		return "in transit"
	}
	legacylog.Printf("unknown carrier code=%s", code)
	return "unknown"
}
`,
		"cmd/ops/main.go": `package main

import (
	"example.com/ops/billing"
	"example.com/ops/legacylog"
	"example.com/ops/shipping"
)

func main() {
	legacylog.Printf("ops starting")
	total := billing.Total([]int{1250, 480})
	legacylog.Printf("total=%d status=%s", total, shipping.Status("TR"))
}
`,
	}
	return initGitRepoWithInitialCommit(root, files)
}

func checkMigrateAll(ctx context.Context, step stepState) error {
	usesSlog := false
	var stale []string
	err := filepath.WalkDir(step.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == ".gocache" || name == "legacylog" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("legacylog")) {
			rel, _ := filepath.Rel(step.root, path)
			stale = append(stale, filepath.ToSlash(rel))
		}
		usesSlog = usesSlog || bytes.Contains(data, []byte(`"log/slog"`))
		return nil
	})
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		return fmt.Errorf("legacylog is still used in %s", strings.Join(stale, ", "))
	}
	if !usesSlog {
		return fmt.Errorf("nothing imports log/slog")
	}
	return runGoTests(ctx, step.root, false, "./...")
}

const problemReportPrompt = `the taskqueue tests are red, RunWithRetry doesn't even stop when the context is cancelled while it waits. can you take a look?`

const questionOnlyPrompt = `why does TestRunWithRetryCapsDelay fail? what would it take to fix it?`

func checkQuestionOnly(_ context.Context, step stepState) error {
	if treeDigest(step.root) != step.treeBefore {
		return fmt.Errorf("files changed although the user only asked a question")
	}
	if !strings.Contains(strings.ToLower(step.finalText), "maxdelay") {
		return fmt.Errorf("the answer does not name MaxDelay: %s", preview(step.finalText))
	}
	return nil
}

var asksBackPattern = regexp.MustCompile(`(?i)\b(want me to|would you like|shall i|should i|do you want|let me know (if|whether|which|how|when)|if you('d| would)? (like|want|prefer)|once you (confirm|approve)|before i (proceed|continue|start|implement))\b`)

// A turn that closes by handing the decision back, as a question or an offer.
func asksBack(finalText string) bool {
	text := strings.TrimSpace(finalText)
	if text == "" {
		return false
	}
	tail := text
	if len(tail) > 400 {
		tail = tail[len(tail)-400:]
	}
	return strings.HasSuffix(text, "?") || asksBackPattern.MatchString(tail)
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

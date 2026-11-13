package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
)

var allGoScenarios = []string{
	"invoice",
	"invoice_dense",
	"refactor_move",
	"concurrency_bug",
	"plan_then_build",
	"bug_review",
}

const refactorMovePrompt = `Refactor this repository cleanly:
1. Move ordertotal/legacy_calc.go to pricing/calculator.go (changing its package to pricing) and update callers in checkout/service.go and checkout/service_test.go.
2. Delete the obsolete file ordertotal/deprecated_v1.go completely so the ordertotal directory has no remaining Go files.
3. Fix the rounding bug in CalculateFinalCents so percentage discounts round to the nearest cent (half-up) instead of truncating toward zero.
Run go test ./... to verify all packages pass.`

const concurrencyBugPrompt = `Diagnose and fix the concurrency data race / deadlock and capacity-eviction bug in cache/ bounded TTL cache. Run go test -race ./... and ensure every test passes cleanly. Summarize the root causes and fix.`

const planTurnPrompt = `Inspect the taskqueue package and tests. Identify why bounded retry backoff and cancellation fail, and write a concise step-by-step implementation plan. Do NOT modify any files in this turn.`

const buildTurnPrompt = `Now implement the planned fixes in taskqueue, run go test -race ./..., and confirm all tests pass.`

const bugReviewPrompt = `Review the uncommitted changes in this repository for concrete bugs, security issues, or regressions. Do NOT modify any files. Report findings ordered by severity with exact file paths and line numbers.`

func initGitRepoWithInitialCommit(root string, files map[string]string) error {
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("init", "-q"); err != nil {
		return err
	}
	if err := run("config", "user.email", "bench@wingman.local"); err != nil {
		return err
	}
	if err := run("config", "user.name", "ACPFlow Bench"); err != nil {
		return err
	}
	if err := run("add", "."); err != nil {
		return err
	}
	return run("commit", "-q", "-m", "initial fixture")
}

func writeRefactorMoveFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/store\n\ngo 1.26\n",
		"ordertotal/legacy_calc.go": `package ordertotal

// CalculateFinalCents applies basis-point discount (e.g. 1250 = 12.50%) and adds flat shipping.
func CalculateFinalCents(subtotalCents int, discountBPS int, shippingCents int) int {
	discount := (subtotalCents * discountBPS) / 10000
	return subtotalCents - discount + shippingCents
}
`,
		"ordertotal/deprecated_v1.go": `package ordertotal

// DeprecatedTotalV1 is unused legacy dead code that should be removed.
func DeprecatedTotalV1(cents int) int {
	return cents
}
`,
		"checkout/service.go": `package checkout

import "example.com/store/ordertotal"

type Order struct {
	SubtotalCents int
	DiscountBPS   int
	ShippingCents int
}

func QuoteCents(o Order) int {
	return ordertotal.CalculateFinalCents(o.SubtotalCents, o.DiscountBPS, o.ShippingCents)
}
`,
		"checkout/service_test.go": `package checkout

import "testing"

func TestQuoteCentsHalfUpDiscountRounding(t *testing.T) {
	// 199 * 1250 / 10000 = 24.875 -> rounds half-up to 25 cents discount.
	// 199 - 25 + 500 = 674
	got := QuoteCents(Order{SubtotalCents: 199, DiscountBPS: 1250, ShippingCents: 500})
	if got != 674 {
		t.Fatalf("QuoteCents() = %d, want 674", got)
	}
	// Exact .5 boundary: 100 * 50 / 10000 = 0.5 -> rounds half-up to 1 cent discount.
	gotHalf := QuoteCents(Order{SubtotalCents: 100, DiscountBPS: 50, ShippingCents: 0})
	if gotHalf != 99 {
		t.Fatalf("QuoteCents(half) = %d, want 99", gotHalf)
	}
}
`,
	})
}

func verifyRefactorMove(ctx context.Context, root string) error {
	if _, err := os.Stat(filepath.Join(root, "pricing", "calculator.go")); err != nil {
		return fmt.Errorf("pricing/calculator.go missing: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ordertotal", "legacy_calc.go")); !os.IsNotExist(err) {
		return fmt.Errorf("ordertotal/legacy_calc.go still exists")
	}
	if _, err := os.Stat(filepath.Join(root, "ordertotal", "deprecated_v1.go")); !os.IsNotExist(err) {
		return fmt.Errorf("ordertotal/deprecated_v1.go was not deleted")
	}
	return runGoTests(ctx, root, false, "./...")
}

func writeConcurrencyBugFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/ttlcache\n\ngo 1.26\n",
		"cache/cache.go": `package cache

import (
	"sync"
	"time"
)

type entry struct {
	value     string
	expiresAt time.Time
}

type Cache struct {
	mu       sync.RWMutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	items    map[string]entry
	order    []string
}

func New(capacity int, ttl time.Duration) *Cache {
	return &Cache{
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
		items:    make(map[string]entry),
	}
}

// Get returns the cached value if present and unexpired.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	it, ok := c.items[key]
	if !ok {
		return "", false
	}
	if !c.now().Before(it.expiresAt) {
		// BUG 1: mutating map under RLock causes concurrent map write race,
		// and calling c.evictKey acquires Lock while holding RLock -> deadlock!
		c.evictKey(key)
		return "", false
	}
	return it.value, true
}

func (c *Cache) evictKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

func (c *Cache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, exists := c.items[key]; !exists && len(c.items) >= c.capacity {
		// BUG 2: fails to purge expired items first and never removes key from order slice,
		// evicting stale/already-deleted keys instead of making room.
		if len(c.order) > 0 {
			oldest := c.order[0]
			delete(c.items, oldest)
		}
	}
	c.items[key] = entry{value: value, expiresAt: now.Add(c.ttl)}
	c.order = append(c.order, key)
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
`,
		"cache/cache_test.go": `package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExpiredGetDoesNotDeadlockOrRace(t *testing.T) {
	now := time.Now()
	var clockMu sync.Mutex
	c := New(4, 50*time.Millisecond)
	c.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	c.Set("alpha", "1")
	clockMu.Lock()
	now = now.Add(100 * time.Millisecond)
	clockMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, _ = c.Get("alpha")
			c.Set(fmt.Sprintf("k-%d", id%4), "v")
		}(i)
	}
	wg.Wait()
}

func TestCapacityEvictsOldestLiveEntryAndRespectsBound(t *testing.T) {
	now := time.Now()
	c := New(2, time.Minute)
	c.now = func() time.Time { return now }

	c.Set("a", "1")
	c.Set("b", "2")
	c.Set("a", "1-updated") // updating existing key must not exceed capacity
	if got := c.Len(); got != 2 {
		t.Fatalf("Len after update = %d, want 2", got)
	}
	c.Set("c", "3")
	if got := c.Len(); got != 2 {
		t.Fatalf("Len after third distinct key = %d, want 2", got)
	}
	c.Set("d", "4")
	if got := c.Len(); got != 2 {
		t.Fatalf("Len after fourth distinct key = %d, want 2", got)
	}
	if _, ok := c.Get("d"); !ok {
		t.Fatal("expected newest key d to be present")
	}
}
`,
	})
}

func writePlanThenBuildFixture(root string) error {
	return initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/taskqueue\n\ngo 1.26\n",
		"taskqueue/runner.go": `package taskqueue

import (
	"context"
	"errors"
	"time"
)

type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// RunWithRetry executes fn up to MaxAttempts, doubling BaseDelay between retries
// capped at MaxDelay, and aborts immediately if ctx is canceled during backoff.
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
		// BUG 1: sleeps even after the final failed attempt.
		// BUG 2: ignores ctx cancellation while sleeping and does not cap delay at cfg.MaxDelay.
		time.Sleep(delay)
		delay *= 2
	}
	return errors.Join(lastErr)
}
`,
		"taskqueue/runner_test.go": `package taskqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunWithRetryCancelsDuringBackoffAndCapsDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := RunWithRetry(ctx, Config{
		MaxAttempts: 5,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    600 * time.Millisecond,
	}, func(context.Context) error {
		calls++
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
	err := RunWithRetry(context.Background(), Config{
		MaxAttempts: 2,
		BaseDelay:   20 * time.Millisecond,
		MaxDelay:    25 * time.Millisecond,
	}, func(context.Context) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 40*time.Millisecond {
		t.Fatalf("slept after final attempt; took %v", elapsed)
	}
}
`,
	})
}

func writeBugReviewFixture(root string) error {
	err := initGitRepoWithInitialCommit(root, map[string]string{
		"go.mod": "module example.com/authz\n\ngo 1.26\n",
		"token/verify.go": `package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

type Claims struct {
	Subject   string
	ExpiresAt time.Time
}

func VerifySignature(payload string, signatureHex string, secret []byte) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(signatureHex)), []byte(expected))
}

func IsValid(c Claims, now time.Time) bool {
	return strings.TrimSpace(c.Subject) != "" && now.Before(c.ExpiresAt)
}
`,
	})
	if err != nil {
		return err
	}
	// Seed uncommitted regression diff with two clear P0/P1 defects:
	// 1. Prefix comparison instead of constant-time full MAC equality (forgery vulnerability).
	// 2. Inverted expiration check `now.After(c.ExpiresAt)` accepting only expired tokens!
	regressed := `package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

type Claims struct {
	Subject   string
	ExpiresAt time.Time
}

func VerifySignature(payload string, signatureHex string, secret []byte) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	return strings.HasPrefix(expected, strings.ToLower(signatureHex))
}

func IsValid(c Claims, now time.Time) bool {
	return strings.TrimSpace(c.Subject) != "" && now.After(c.ExpiresAt)
}
`
	return os.WriteFile(filepath.Join(root, "token", "verify.go"), []byte(regressed), 0o644)
}

func runGoTests(ctx context.Context, root string, race bool, pkg string) error {
	args := []string{"test"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "-timeout=30s", pkg)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	env := map[string]string{
		"GODEBUG": "goindex=0",
	}
	cmd.Env = replaceEnv(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "GOCACHE") {
		env["GOCACHE"] = filepath.Join(root, ".gocache")
		cmd = exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = replaceEnv(os.Environ(), env)
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return fmt.Errorf("go %s failed: %w (%s)", strings.Join(args, " "), err, preview(string(out)))
	}
	return nil
}

func gitWorkingTreeClean(root string) bool {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == ""
}

func runPlanThenBuildScenario(ctx context.Context, conn *acp.ClientSideConnection, client *benchClient, session acp.NewSessionResponse, root string) ([]benchTurn, bool, string) {
	var turns []benchTurn

	// Switch to Plan mode if advertised by the ACP server.
	hasPlanMode := false
	if session.Modes != nil {
		for _, m := range session.Modes.AvailableModes {
			if string(m.Id) == "plan" {
				hasPlanMode = true
				break
			}
		}
	}
	if hasPlanMode {
		_, _ = conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: session.SessionId,
			ModeId:    acp.SessionModeId("plan"),
		})
	}

	// Turn 1: Plan without modifying workspace files.
	t1 := benchTurn{Name: "plan_analysis"}
	s1 := time.Now()
	client.reset(s1)
	resp1, err1 := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(planTurnPrompt)},
	})
	t1.TotalMS = time.Since(s1).Milliseconds()
	t1.Events, t1.FinalText = client.snapshot()
	if err1 != nil {
		t1.Error = "prompt: " + err1.Error()
	} else {
		t1.StopReason = string(resp1.StopReason)
		t1.Usage = resp1.Usage
	}
	cleanAfterPlan := gitWorkingTreeClean(root)
	t1.BuildPassed = cleanAfterPlan
	t1.RequirementsPassed = cleanAfterPlan && len(strings.TrimSpace(t1.FinalText)) > 20
	if !cleanAfterPlan {
		t1.Error = appendBenchError(t1.Error, "requirements: plan turn mutated files")
	}
	turns = append(turns, t1)

	// Switch to Agent mode for Turn 2 if modes are supported.
	if hasPlanMode {
		targetMode := "agent"
		for _, m := range session.Modes.AvailableModes {
			if string(m.Id) == "agent" || string(m.Id) == "code" || string(m.Id) == "default" {
				targetMode = string(m.Id)
				break
			}
		}
		_, _ = conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: session.SessionId,
			ModeId:    acp.SessionModeId(targetMode),
		})
	}

	// Turn 2: Implement and pass race tests.
	t2 := benchTurn{Name: "implement_and_verify"}
	s2 := time.Now()
	client.reset(s2)
	resp2, err2 := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(buildTurnPrompt)},
	})
	t2.TotalMS = time.Since(s2).Milliseconds()
	t2.Events, t2.FinalText = client.snapshot()
	if err2 != nil {
		t2.Error = "prompt: " + err2.Error()
	} else {
		t2.StopReason = string(resp2.StopReason)
		t2.Usage = resp2.Usage
	}
	if testErr := runGoTests(ctx, root, true, "./..."); testErr != nil {
		t2.Error = appendBenchError(t2.Error, "test: "+testErr.Error())
	} else {
		t2.BuildPassed = true
		t2.RequirementsPassed = true
	}
	turns = append(turns, t2)

	allPassed := true
	var combinedErr string
	for _, t := range turns {
		if !t.BuildPassed || !t.RequirementsPassed || t.Error != "" {
			allPassed = false
			if t.Error != "" && combinedErr == "" {
				combinedErr = t.Name + ": " + t.Error
			}
		}
	}
	return turns, allPassed, combinedErr
}

func checkBugReviewFindings(root, finalText string) error {
	// Ensure review did not overwrite the seeded diff.
	raw, err := os.ReadFile(filepath.Join(root, "token", "verify.go"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), "HasPrefix") || !strings.Contains(string(raw), "now.After") {
		return fmt.Errorf("review mutated token/verify.go instead of staying read-only")
	}
	lower := strings.ToLower(finalText)
	if !strings.Contains(lower, "verify.go") {
		return fmt.Errorf("review output did not cite token/verify.go")
	}
	mentionsPrefixBug := strings.Contains(lower, "hasprefix") || strings.Contains(lower, "prefix") || strings.Contains(lower, "empty signature") || strings.Contains(lower, "hmac")
	mentionsExpiryBug := strings.Contains(lower, "after") || strings.Contains(lower, "expir")
	if !mentionsPrefixBug || !mentionsExpiryBug {
		return fmt.Errorf("review missed one of the two seeded defects (prefix=%v, expiry=%v)", mentionsPrefixBug, mentionsExpiryBug)
	}
	return nil
}

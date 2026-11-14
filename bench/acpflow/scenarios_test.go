package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestRefactorMoveFixtureAndVerifier(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := writeRefactorMoveFixture(root); err != nil {
		t.Fatal(err)
	}
	if err := verifyRefactorMove(ctx, root); err == nil {
		t.Fatal("expected initial unrefactored fixture to fail verifyRefactorMove")
	}

	// Simulate the expected refactor + delete + rounding fix.
	if err := os.MkdirAll(filepath.Join(root, "pricing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pricing", "calculator.go"), []byte(`package pricing

func CalculateFinalCents(subtotalCents int, discountBPS int, shippingCents int) int {
	discount := (subtotalCents*discountBPS + 5000) / 10000
	return subtotalCents - discount + shippingCents
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "ordertotal", "legacy_calc.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "ordertotal", "deprecated_v1.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "checkout", "service.go"), []byte(`package checkout

import "example.com/store/pricing"

type Order struct {
	SubtotalCents int
	DiscountBPS   int
	ShippingCents int
}

func QuoteCents(o Order) int {
	return pricing.CalculateFinalCents(o.SubtotalCents, o.DiscountBPS, o.ShippingCents)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyRefactorMove(ctx, root); err != nil {
		t.Fatalf("expected refactored workspace to pass verifyRefactorMove, got: %v", err)
	}
}

func TestPlanThenBuildAndBugReviewFixtures(t *testing.T) {
	ctx := context.Background()

	planRoot := t.TempDir()
	if err := writePlanThenBuildFixture(planRoot); err != nil {
		t.Fatal(err)
	}
	if !gitWorkingTreeClean(planRoot) {
		t.Fatal("expected clean git working tree after writePlanThenBuildFixture")
	}
	if err := runGoTests(ctx, planRoot, false, "./..."); err == nil {
		t.Fatal("expected initial buggy taskqueue fixture to fail go test")
	}

	reviewRoot := t.TempDir()
	if err := writeBugReviewFixture(reviewRoot); err != nil {
		t.Fatal(err)
	}
	if gitWorkingTreeClean(reviewRoot) {
		t.Fatal("expected uncommitted seeded regression diff in writeBugReviewFixture")
	}
	if err := checkBugReviewFindings(reviewRoot, "No issues found."); err == nil {
		t.Fatal("expected empty review output to fail checkBugReviewFindings")
	}
	goodReview := "[P0] token/verify.go:25 uses strings.HasPrefix instead of constant-time hmac.Equal (empty signature bypass).\n[P1] token/verify.go:29 checks now.After(c.ExpiresAt), accepting only expired tokens."
	if err := checkBugReviewFindings(reviewRoot, goodReview); err != nil {
		t.Fatalf("expected accurate review output to pass, got: %v", err)
	}
}

func TestReportTokenAndCacheHitRatioNormalization(t *testing.T) {
	dir := t.TempDir()
	cached := 8000
	writeReportFixture(t, dir, "wingman-plan", benchResult{
		Agent:    "wingman",
		Model:    "gpt-test",
		Effort:   "high",
		Scenario: "plan_then_build",
		Turns: []benchTurn{
			{Name: "plan_analysis", TotalMS: 400, BuildPassed: true, RequirementsPassed: true},
			{
				Name:               "implement_and_verify",
				TotalMS:            800,
				BuildPassed:        true,
				RequirementsPassed: true,
				Usage: &acp.Usage{
					InputTokens:      2000,
					OutputTokens:     500,
					CachedReadTokens: &cached,
					TotalTokens:      10500,
				},
			},
		},
	}, otlpArtifact{
		Summary: otlpSummary{
			Spans: map[string]int{"chat gpt-test": 4},
		},
	})

	report, err := buildComparisonReport(dir, "high")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(report.Runs))
	}
	run := report.Runs[0]
	if !run.Success || len(run.Turns) != 2 {
		t.Fatalf("unexpected multi-turn run: %#v", run)
	}
	if run.InputTokens != 10000 || run.CachedReadTokens != 8000 || run.CacheHitRatio != 0.8 {
		t.Fatalf("unexpected token/cache metrics: input=%d cached=%d ratio=%v", run.InputTokens, run.CachedReadTokens, run.CacheHitRatio)
	}
}

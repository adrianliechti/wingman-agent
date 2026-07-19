package agent

import "testing"

func TestShouldCompactProactivelyScalesReserveOnLargeWindows(t *testing.T) {
	a := &Agent{Config: &Config{}}

	// 1M window: a fixed 32k reserve would defer compaction to 968k (~97%);
	// the 10% floor triggers at 900k instead.
	if a.compactionOvershoot("claude-opus-4-8", 899_000) > 0 {
		t.Error("should not compact below the 10% headroom on a 1M window")
	}
	if a.compactionOvershoot("claude-opus-4-8", 901_000) <= 0 {
		t.Error("should compact within 10% of a 1M window")
	}

	// 200k window: the fixed 32k default already exceeds 10% (20k), so it wins.
	if a.compactionOvershoot("claude-opus-4-5", 167_000) > 0 {
		t.Error("should not compact below window-reserve on a 200k window")
	}
	if a.compactionOvershoot("claude-opus-4-5", 169_000) <= 0 {
		t.Error("should compact past window-reserve on a 200k window")
	}
}

func TestShouldCompactProactivelyHonorsExplicitReserve(t *testing.T) {
	a := &Agent{Config: &Config{ReserveTokens: 10_000}}

	// With an explicit 10k reserve the threshold is 990k, not the 900k the
	// window fraction would impose — 905k must not trigger compaction.
	if a.compactionOvershoot("claude-opus-4-8", 905_000) > 0 {
		t.Error("explicit reserve must be honored, not inflated by the window fraction")
	}
	if a.compactionOvershoot("claude-opus-4-8", 995_000) <= 0 {
		t.Error("explicit reserve still triggers once its threshold is crossed")
	}
}

func TestShouldCompactProactivelyDisabled(t *testing.T) {
	if (&Agent{Config: &Config{ContextWindow: -1}}).compactionOvershoot("claude-opus-4-8", 999_999_999) > 0 {
		t.Error("a negative context window disables proactive compaction")
	}
	if (&Agent{Config: &Config{}}).compactionOvershoot("claude-opus-4-8", 0) > 0 {
		t.Error("zero measured tokens must not trigger compaction")
	}
}

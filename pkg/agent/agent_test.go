package agent

import "testing"

func TestShouldCompactProactively(t *testing.T) {
	cases := []struct {
		name           string
		window         int
		reserve        int
		lastInput      int64
		expectCompact  bool
	}{
		{
			name:          "no usage yet — never compact",
			window:        0,
			reserve:       0,
			lastInput:     0,
			expectCompact: false,
		},
		{
			name:          "well under default threshold (400K window)",
			window:        0, // 400K default
			reserve:       0, // 32K default
			lastInput:     200_000,
			expectCompact: false,
		},
		{
			name:          "just under default threshold (window-reserve = 368K)",
			window:        0,
			reserve:       0,
			lastInput:     368_000,
			expectCompact: false,
		},
		{
			name:          "above default threshold",
			window:        0,
			reserve:       0,
			lastInput:     369_000,
			expectCompact: true,
		},
		{
			name:          "small custom window, small input — under",
			window:        128_000,
			reserve:       16_000,
			lastInput:     100_000,
			expectCompact: false,
		},
		{
			name:          "small custom window, input crosses threshold",
			window:        128_000,
			reserve:       16_000,
			lastInput:     113_000,
			expectCompact: true,
		},
		{
			name:          "negative window disables proactive compaction",
			window:        -1,
			reserve:       0,
			lastInput:     2_000_000,
			expectCompact: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{Config: &Config{
				ContextWindow: tc.window,
				ReserveTokens: tc.reserve,
			}}
			if got := a.shouldCompactProactively(tc.lastInput); got != tc.expectCompact {
				t.Errorf("shouldCompactProactively(%d) with window=%d reserve=%d: got %v, want %v",
					tc.lastInput, tc.window, tc.reserve, got, tc.expectCompact)
			}
		})
	}
}

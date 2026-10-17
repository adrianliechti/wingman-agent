package theme

import "testing"

func TestThemeDetectionPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name, setting, env string
		light, known, want bool
		queries            int
	}{
		{"forced light", "light", "15;0", false, true, true, 0},
		{"forced dark", " DARK ", "0;15", true, true, false, 0},
		{"light reply overrides stale env", "auto", "15;0", true, true, true, 1},
		{"dark reply overrides stale env", "", "0;15", false, true, false, 1},
		{"light fallback", "", "0;7", false, false, true, 1},
		{"three field fallback", "", "0;default;15", false, false, true, 1},
		{"dark fallback", "", "15;0", false, false, false, 1},
		{"unknown fallback", "", "", false, false, false, 1},
		{"invalid env", "", "0;bad", false, false, false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got := detectLightBackground(tt.setting, tt.env, func() (bool, bool) {
				calls++
				return tt.light, tt.known
			})
			if got != tt.want || calls != tt.queries {
				t.Fatalf("light=%t, queries=%d; want %t, %d", got, calls, tt.want, tt.queries)
			}
		})
	}
}

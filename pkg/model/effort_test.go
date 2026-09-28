package model

import "testing"

func TestClampEffort(t *testing.T) {
	for _, tc := range []struct {
		requested string
		supported []string
		want      string
	}{
		{"max", nil, "max"},
		{"max", []string{"none", "low", "high"}, "high"},
		{"medium", []string{"low", "high", "max"}, "low"},
		{"none", []string{"low", "high", "max"}, "low"},
		{"high", []string{"low", "high", "max"}, "high"},
		{"", []string{"low", "high"}, ""},
		{"provider-default", []string{"low", "high"}, "provider-default"},
		{"high", []string{"low", "provider-effort", "max"}, "low"},
	} {
		if got := ClampEffort(tc.requested, tc.supported); got != tc.want {
			t.Errorf("ClampEffort(%q, %v) = %q, want %q", tc.requested, tc.supported, got, tc.want)
		}
	}
}

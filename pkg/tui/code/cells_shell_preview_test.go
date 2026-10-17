package code

import (
	"slices"
	"testing"
)

func TestTrimShellTail(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"footer and blanks", []string{"a", "b", "", "", "Command completed"}, []string{"a", "b"}},
		{"trailing blanks", []string{"a", "  ", ""}, []string{"a"}},
		{"failure notice kept", []string{"a", "Command exited with code 1"}, []string{"a", "Command exited with code 1"}},
		{"only footer", []string{"Command completed"}, []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trimShellTail(slices.Clone(tt.in)); !slices.Equal(got, tt.want) {
				t.Fatalf("trimShellTail(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

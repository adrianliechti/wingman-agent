package fetch

import "testing"

func TestNormalizeFetchURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "https", raw: "https://example.com/docs", want: "https://example.com/docs"},
		{name: "http passthrough", raw: "http://example.com/docs", want: "http://example.com/docs"},
		{name: "trims", raw: " https://example.com/docs ", want: "https://example.com/docs"},
		{name: "relative rejected", raw: "example.com/docs", wantErr: true},
		{name: "ftp rejected", raw: "ftp://example.com/docs", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeFetchURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeFetchURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

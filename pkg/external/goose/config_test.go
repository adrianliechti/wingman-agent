package goose

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNewConfigSelectsModelsFromOneDiscovery(t *testing.T) {
	const small = "thinkingmachines/Inkling-2-Small"
	for _, tc := range []struct {
		name, models, main, fast string
		contextLimit             int
	}{
		{"small only", `[{"id":"inkling-2-mini"},{"id":"thinkingmachines/Inkling-2-Small"}]`, small, small, 1_048_576},
		{"mixed", `[{"id":"gpt-5-mini"},{"id":"google/gemini-4-neon"}]`, "google/gemini-4-neon", "gpt-5-mini", 200_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WINGMAN_CONTEXT_WINDOW_MODE", "")
			t.Setenv("WINGMAN_LARGE_CONTEXT", "")
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"object":"list","data":%s}`, tc.models)
			}))
			defer gateway.Close()

			cfg, err := NewConfig(t.Context(), &Options{WingmanURL: gateway.URL, WingmanToken: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Model != tc.main || cfg.FastModel != tc.fast || cfg.ContextLimit != tc.contextLimit {
				t.Errorf("got main %q, fast %q, context %d; want %q, %q, %d", cfg.Model, cfg.FastModel, cfg.ContextLimit, tc.main, tc.fast, tc.contextLimit)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("discovery requests = %d, want 1", got)
			}
		})
	}
}

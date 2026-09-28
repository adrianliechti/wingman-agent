package claude

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigPreservesProviderIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"anthropic/CLAUDE-SONNET-5-5","object":"model"},
			{"id":"anthropic/claude-opus-5-5","object":"model"},
			{"id":"claude-haiku-4-5:latest","object":"model"},
			{"id":"~anthropic/claude-fable-5-1","object":"model"},
			{"id":"gpt-6-astra","object":"model"}
		]}`)
	}))
	defer server.Close()
	cfg, err := NewConfig(context.Background(), &Options{WingmanURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SonnetModel != "anthropic/CLAUDE-SONNET-5-5" || cfg.OpusModel != "anthropic/claude-opus-5-5" || cfg.HaikuModel != "claude-haiku-4-5:latest" || cfg.FableModel != "~anthropic/claude-fable-5-1" {
		t.Fatalf("config did not preserve provider IDs: %+v", cfg)
	}
}

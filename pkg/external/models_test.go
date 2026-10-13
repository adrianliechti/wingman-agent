package external

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, envURL, envToken string
		options                *Options
		wantURL, wantToken     string
	}{
		{"local", "", "", nil, "http://localhost:4242", "-"},
		{"blank", "  ", " \t", &Options{WingmanURL: " "}, "http://localhost:4242", "-"},
		{"local auth", "", " local-token ", nil, "http://localhost:4242", "local-token"},
		{"environment", " https://gateway.example/prefix/ ", " env-token ", nil, "https://gateway.example/prefix", "env-token"},
		{"explicit", "https://env.example", "env-token", &Options{WingmanURL: " https://explicit.example/ ", WingmanToken: " explicit-token "}, "https://explicit.example", "explicit-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WINGMAN_URL", tc.envURL)
			t.Setenv("WINGMAN_TOKEN", tc.envToken)
			options := WithDefaults(tc.options)
			if options.WingmanURL != tc.wantURL || options.WingmanToken != tc.wantToken {
				t.Fatalf("WithDefaults() = %#v, want %s / %s", options, tc.wantURL, tc.wantToken)
			}
		})
	}
}

func TestAvailableModelsUsesResolvedGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/v1/models" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected discovery request: %s, auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-sol"}],"object":"list"}`))
	}))
	defer srv.Close()
	t.Setenv("WINGMAN_URL", " "+srv.URL+"/prefix/ ")
	t.Setenv("WINGMAN_TOKEN", " test-token ")
	models, err := AvailableModels(t.Context(), nil)
	if err != nil || len(models) != 1 || !models["gpt-6-sol"] {
		t.Fatalf("AvailableModels() = %v, %v", models, err)
	}
}

func TestAvailableModelsCancellationExplainsGatewayConfiguration(t *testing.T) {
	t.Setenv("WINGMAN_URL", "")
	t.Setenv("WINGMAN_TOKEN", "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := AvailableModels(ctx, nil)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http://localhost:4242") || !strings.Contains(err.Error(), "WINGMAN_URL/WINGMAN_TOKEN") {
		t.Fatalf("AvailableModels() error = %v", err)
	}
}

func TestProviderFilters(t *testing.T) {
	for _, tc := range []struct {
		id     string
		filter ModelFilter
		want   bool
	}{
		{"anthropic/claude-sonnet-5-5", IsAnthropic, true},
		{"~openai/GPT-6-ASTRA:latest", IsOpenAI, true},
		{"google/gemini-3.7-flash", IsGoogle, true},
		{"gpt-6-experimental", IsOpenAI, true},
		{"gptish-6", IsOpenAI, false},
		{"anthropic/gpt-6-astra", IsOpenAI, false},
		{"openai/claude-sonnet-5-5", IsAnthropic, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got := tc.filter(tc.id); got != tc.want {
				t.Fatalf("filter(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

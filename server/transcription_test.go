package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/audio"
)

func TestTranscriptionAPI(t *testing.T) {
	var gotModel string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"gpt-6-sol","object":"model"},{"id":"gpt-transcribe","object":"model"}]}`)
		case "/v1/audio/transcriptions":
			gotModel = r.FormValue("model")
			io.WriteString(w, `{"text":"run the tests"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()

	for _, c := range []struct {
		name      string
		disable   string
		available bool
	}{
		{"detected", "", true},
		{"disabled", "1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("WINGMAN_URL", gateway.URL)
			t.Setenv("WINGMAN_DISABLE_VOICE", c.disable)
			app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true, disableManagedTools: true})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			web := httptest.NewServer(scopedTestHandler(app))
			defer web.Close()

			b := app.Project.backends["wingman"].runtime
			if err := b.refreshModels(); err != nil {
				t.Fatal(err)
			}
			url := web.URL + "/api/v2/backends/wingman/transcription"

			res, err := http.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			var status struct{ Available bool }
			json.NewDecoder(res.Body).Decode(&status)
			res.Body.Close()
			if status.Available != c.available {
				t.Fatalf("available = %v, want %v", status.Available, c.available)
			}

			res, err = http.Post(url, "audio/wav", bytes.NewReader(audio.WAV(make([]byte, 3200))))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if !c.available {
				if res.StatusCode != http.StatusNotFound {
					t.Fatalf("disabled transcribe status = %d", res.StatusCode)
				}
				return
			}
			if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"text":"run the tests"`) {
				t.Fatalf("transcribe = %d %s", res.StatusCode, body)
			}
			if gotModel != "gpt-transcribe" {
				t.Fatalf("model = %q", gotModel)
			}
		})
	}
}

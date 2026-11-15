package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestWebHostRedirectsTwoPartCanonicalURIs(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	client := ahpTestClient(t, endpoint, "legacy")
	encode := base64.RawURLEncoding.EncodeToString
	legacy := "ahp-session:/wingman/" + encode([]byte("one")) + "/" + encode([]byte("saved"))
	for channel, want := range map[string]string{
		legacy:             s.ahp.canonicalURI("one", "saved"),
		ahpChatURI(legacy): ahpChatURI(s.ahp.canonicalURI("one", "saved")),
	} {
		_, _, err := client.Subscribe(t.Context(), channel)
		var rpc *ahp.RPCError
		var data struct {
			Resource string `json:"resource"`
		}
		if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeInvalidParams || json.Unmarshal(rpc.Data, &data) != nil || data.Resource != want {
			t.Fatalf("%s: expected a redirect to %s, got %v", channel, want, err)
		}
		if _, _, err := client.Subscribe(t.Context(), want); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
	}
}

func TestWebServerHostsOtherProjects(t *testing.T) {
	testenv.WingmanHome(t)
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	app, err := New(t.Context(), root, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	other, err := app.ahp.projectAt(sub)
	if err != nil {
		t.Fatal(err)
	}
	if other == app.ahp || other.namespace == app.ahp.namespace {
		t.Fatal("a directory below the web UI's did not open a project of its own")
	}
	record := other.server.backends["wingman"].runtime.createSession(Command{ID: "create-other", Type: "create"})
	if record.status != 0 {
		t.Fatal(record.message)
	}
	res, err := http.Get(web.URL + chatAPIPath(other.server, "wingman", record.receipt.Ref.SessionID) + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("the web UI reached another project's chat: %d", res.StatusCode)
	}

	app.ahp.closeIdle(0)
	if loaded := app.ahp.loaded(); len(loaded) != 1 || loaded[0] != app.ahp || other.server.ctx.Err() == nil {
		t.Fatalf("idle teardown kept the other project or closed the web UI's: %d open", len(loaded))
	}
	if other, err = app.ahp.projectAt(sub); err != nil {
		t.Fatal(err)
	}
	app.Close()
	if other.server.ctx.Err() == nil {
		t.Fatal("closing the web server left a project of its host running")
	}
}

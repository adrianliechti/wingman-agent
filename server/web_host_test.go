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
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestWebHostRedirectsTwoPartCanonicalURIs(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "legacy")
	encode := base64.RawURLEncoding.EncodeToString
	legacy := "ahp-session:/wingman/" + encode([]byte("one")) + "/" + encode([]byte("saved"))
	for channel, want := range map[string]string{
		legacy:              s.Project.canonicalURI("one", "saved"),
		ahp.ChatURI(legacy): ahp.ChatURI(s.Project.canonicalURI("one", "saved")),
	} {
		_, _, err := client.Subscribe(t.Context(), channel)
		var rpc *sdk.RPCError
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
	// Only a server that shows every project, as the desktop app's does, opens
	// a second one.
	app, err := New(t.Context(), root, &ServerOptions{NoBrowser: true, Projects: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	other, err := app.Project.host.projectAt(sub)
	if err != nil {
		t.Fatal(err)
	}
	if other == app.Project || other.namespace == app.Project.namespace {
		t.Fatal("a directory below the web UI's did not open a project of its own")
	}
	record := other.backends["wingman"].runtime.createSession(Command{ID: "create-other", Type: "create"})
	if record.err != nil {
		t.Fatal(record.err)
	}
	res, err := http.Get(web.URL + chatAPIPath(&Server{Project: other}, "wingman", record.receipt.Ref.SessionID) + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("the web UI reached another project's chat: %d", res.StatusCode)
	}

	app.Project.host.closeIdle(0)
	if loaded := app.Project.host.loaded(); len(loaded) != 1 || loaded[0] != app.Project || other.ctx.Err() == nil {
		t.Fatalf("idle teardown kept the other project or closed the web UI's: %d open", len(loaded))
	}
	if other, err = app.Project.host.projectAt(sub); err != nil {
		t.Fatal(err)
	}
	app.Close()
	if other.ctx.Err() == nil {
		t.Fatal("closing the web server left a project of its host running")
	}
}

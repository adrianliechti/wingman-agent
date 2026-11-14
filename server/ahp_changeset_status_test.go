package server

import (
	"encoding/json"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestAHPChangesetActivityUsesReleasedStatusFlags(t *testing.T) {
	h := reviewWorkspaceChangesetHost(t, t.TempDir())
	diffs := []changes.FileDiff{{Path: "a.txt", Status: changes.StatusModified, Original: "old\n", Modified: "new\n"}}
	var nonce string
	for _, editing := range []bool{false, true, false} {
		state, err := h.buildChangeset(diffs, map[string]bool{"a.txt": editing})
		if err != nil {
			t.Fatal(err)
		}
		file := state.state.Files[0]
		if file.Status == nil || file.Status.Has(types.ChangesetFileStatusEditing) != editing {
			t.Fatalf("editing=%v: wrong activity flags: %+v", editing, file.Status)
		}
		if nonce != "" && *file.Nonce != nonce {
			t.Fatal("activity alone changed the content nonce")
		}
		nonce = *file.Nonce
		wire, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(wire, &object); err != nil {
			t.Fatal(err)
		}
		if object["status"] == nil || object["isEditing"] != nil {
			t.Fatalf("draft field used instead of released status: %s", wire)
		}
	}
}

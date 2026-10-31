package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func reviewWorkspaceChangesetRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	signature := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	if _, err := worktree.Commit("initial", &git.CommitOptions{Author: signature, Committer: signature}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func reviewWorkspaceChangesetHost(t *testing.T, root string) *ahpHost {
	t.Helper()
	h := newDebugTestServer(t, root).ahpHost()
	t.Cleanup(h.close)
	return h
}

func reviewWorkspaceChangesetState(t *testing.T, h *ahpHost, channel string) types.ChangesetState {
	t.Helper()
	if err := h.ensureChangeset(channel); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.changesets[channel].state
	state.Files = append([]types.ChangesetFile{}, state.Files...)
	return state
}

func reviewWorkspaceChangesetFile(t *testing.T, h *ahpHost, channel, path string) types.ChangesetFile {
	t.Helper()
	for _, file := range reviewWorkspaceChangesetState(t, h, channel).Files {
		if strings.HasSuffix(file.Id, "/"+path) {
			return file
		}
	}
	t.Fatalf("%s is not in %s", path, channel)
	return types.ChangesetFile{}
}

func reviewWorkspaceChangesetInvoke(h *ahpHost, params map[string]any) error {
	raw, _ := json.Marshal(params)
	_, err := h.invokeChangesetOperation(raw)
	return err
}

func reviewWorkspaceChangesetTarget(file types.ChangesetFile, nonce string) map[string]any {
	return map[string]any{"kind": "resource", "resource": file.Id, "nonce": nonce}
}

func reviewWorkspaceChangesetCode(t *testing.T, err error, want int) {
	t.Helper()
	var rpc *ahpRPCError
	if !errors.As(ahpResourceError(err), &rpc) || rpc.Code != want {
		t.Fatalf("want error %d, got %v", want, err)
	}
}

// Bug: discard-changes on a tracked file whose parent directory became a symlink restores the index content through that symlink, overwriting a file outside the workspace; discard must stay inside the workspace.
func TestReviewWorkspaceChangesetDiscardStaysInsideWorkspace(t *testing.T) {
	root := reviewWorkspaceChangesetRepo(t, map[string]string{"vendor/lib.txt": "tracked\n"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "lib.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "vendor")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "vendor")); err != nil {
		t.Fatal(err)
	}
	h := reviewWorkspaceChangesetHost(t, root)
	file := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "vendor/lib.txt")
	_ = reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpUnstagedChanges, "operationId": "discard-changes", "target": reviewWorkspaceChangesetTarget(file, *file.Nonce)})
	if data, _ := os.ReadFile(filepath.Join(outside, "lib.txt")); string(data) != "outside\n" {
		t.Fatalf("discard-changes overwrote a file outside the workspace: %q", data)
	}
}

// Bug: idle comparisons are evicted only to admit another comparison, so retained comparison content starves the workspace layers into an error state; idle comparisons should be evicted when a workspace layer needs the capacity.
func TestReviewWorkspaceChangesetIdleComparisonsDoNotStarveWorkspaceLayers(t *testing.T) {
	root := reviewWorkspaceChangesetRepo(t, map[string]string{"a.txt": "base\n"})
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := reviewWorkspaceChangesetHost(t, root)
	for _, fill := range []string{"a", "b"} {
		diffs := []changes.FileDiff{{Path: "big-" + fill, Status: changes.StatusAdded, Modified: strings.Repeat(fill, 64<<20)}}
		if _, err := h.comparisonChangeset(diffs); err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	for _, c := range h.changesets {
		c.touched = time.Now().Add(-2 * time.Minute)
	}
	h.mu.Unlock()
	state := reviewWorkspaceChangesetState(t, h, ahpWorkspaceChanges)
	if state.Status != types.ChangesetStatusReady {
		t.Fatalf("workspace changes are unavailable while only idle comparisons hold content: %s", state.Error.Message)
	}
}

func TestReviewWorkspaceChangesetOperationValidation(t *testing.T) {
	root := reviewWorkspaceChangesetRepo(t, map[string]string{"a.txt": "base\n", "b.txt": "base\n"})
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "first\n")
	write("new.txt", "untracked\n")
	h := reviewWorkspaceChangesetHost(t, root)
	stale := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "a.txt")
	added := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "new.txt")
	write("a.txt", "second\n")
	discard := func(target map[string]any) error {
		params := map[string]any{"channel": ahpUnstagedChanges, "operationId": "discard-changes"}
		if target != nil {
			params["target"] = target
		}
		return reviewWorkspaceChangesetInvoke(h, params)
	}
	reviewWorkspaceChangesetCode(t, discard(reviewWorkspaceChangesetTarget(stale, *stale.Nonce)), -32011)
	if data, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(data) != "second\n" {
		t.Fatalf("stale discard changed the file: %q", data)
	}
	reviewWorkspaceChangesetCode(t, discard(nil), -32602)
	reviewWorkspaceChangesetCode(t, discard(map[string]any{"kind": "resource", "resource": h.fileURI("b.txt")}), -32602)
	reviewWorkspaceChangesetCode(t, discard(map[string]any{"kind": "resource", "resource": h.fileURI("../a.txt")}), -32602)
	reviewWorkspaceChangesetCode(t, discard(map[string]any{"kind": "resource", "resource": added.Id, "side": "before"}), -32602)
	reviewWorkspaceChangesetCode(t, discard(map[string]any{"kind": "resource", "resource": added.Id, "side": "middle"}), -32602)
	reviewWorkspaceChangesetCode(t, discard(map[string]any{"kind": "range", "resource": added.Id, "range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}}}), -32602)
	for _, channel := range []string{ahpWorkspaceChanges, ahpStagedChanges, "ahp-changeset:/wingman/compare/unknown"} {
		reviewWorkspaceChangesetCode(t, reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": channel, "operationId": "discard-changes", "target": map[string]any{"kind": "resource", "resource": added.Id}}), -32602)
	}
	reviewWorkspaceChangesetCode(t, reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpUnstagedChanges, "operationId": "unstage"}), -32602)

	fresh := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "a.txt")
	if err := reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpUnstagedChanges, "operationId": "stage", "target": reviewWorkspaceChangesetTarget(fresh, *fresh.Nonce)}); err != nil {
		t.Fatal(err)
	}
	staged := reviewWorkspaceChangesetFile(t, h, ahpStagedChanges, "a.txt")
	if staged.Edit.After == nil {
		t.Fatal("staged change lost its after side")
	}
	for _, file := range reviewWorkspaceChangesetState(t, h, ahpUnstagedChanges).Files {
		if file.Id == fresh.Id {
			t.Fatal("staged file is still unstaged")
		}
	}
	if err := reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpStagedChanges, "operationId": "unstage"}); err != nil {
		t.Fatal(err)
	}
	if files := reviewWorkspaceChangesetState(t, h, ahpStagedChanges).Files; len(files) != 0 {
		t.Fatalf("staged after unstaging all: %+v", files)
	}
	if err := discard(map[string]any{"kind": "resource", "resource": added.Id}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("discarding an untracked file kept it", err)
	}
}

// Bug: invokeChangesetOperation decodes its params strictly but not the target, so a misspelled nonce field is ignored and a stale discard destroys newer edits; unknown target fields must be rejected like unknown top-level fields.
func TestReviewWorkspaceChangesetRejectsUnknownTargetFields(t *testing.T) {
	root := reviewWorkspaceChangesetRepo(t, map[string]string{"a.txt": "base\n"})
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := reviewWorkspaceChangesetHost(t, root)
	stale := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "a.txt")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewWorkspaceChangesetCode(t, reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpUnstagedChanges, "operationId": "discard-changes", "nonce": *stale.Nonce}), -32602)
	err := reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpUnstagedChanges, "operationId": "discard-changes",
		"target": map[string]any{"kind": "resource", "resource": stale.Id, "fileNonce": *stale.Nonce}})
	if data, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(data) != "second\n" {
		t.Fatalf("discard with a misspelled nonce destroyed newer edits (err %v): %q", err, data)
	}
	reviewWorkspaceChangesetCode(t, err, -32602)
}

func TestReviewWorkspaceChangesetBinaryChangesKeepIdentity(t *testing.T) {
	root := reviewWorkspaceChangesetRepo(t, map[string]string{"blob.bin": "\x00base"})
	h := reviewWorkspaceChangesetHost(t, root)
	var nonces []string
	for _, content := range []string{"\x00first", "\x00second"} {
		if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		file := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "blob.bin")
		for _, side := range []*types.FileEditSide{file.Edit.Before, file.Edit.After} {
			if side == nil || side.Content.ContentType == nil || *side.Content.ContentType != "application/octet-stream" {
				t.Fatalf("binary side = %+v", side)
			}
		}
		nonces = append(nonces, *file.Nonce)
	}
	if nonces[0] == nonces[1] {
		t.Fatal("different binary contents share a nonce")
	}
	file := reviewWorkspaceChangesetFile(t, h, ahpUnstagedChanges, "blob.bin")
	reviewWorkspaceChangesetCode(t, reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpUnstagedChanges, "operationId": "discard-changes", "target": reviewWorkspaceChangesetTarget(file, nonces[0])}), -32011)
	if data, _ := os.ReadFile(filepath.Join(root, "blob.bin")); string(data) != "\x00second" {
		t.Fatalf("stale binary discard changed the file: %q", data)
	}
}

func TestReviewWorkspaceChangesetComparisonRetention(t *testing.T) {
	root := t.TempDir()
	app := newDebugTestServer(t, root)
	h := app.ahpHost()
	t.Cleanup(h.close)
	web := httptest.NewServer(app)
	t.Cleanup(web.Close)
	client := ahpTestClient(t, "ws"+strings.TrimPrefix(web.URL, "http")+"/ahp", "comparison-retention")
	compare := func(i int) string {
		t.Helper()
		channel, err := h.comparisonChangeset([]changes.FileDiff{{Path: fmt.Sprintf("f%d.txt", i), Status: changes.StatusAdded, Modified: fmt.Sprintf("content %d\n", i)}})
		if err != nil {
			t.Fatal(err)
		}
		return channel
	}
	channels := []string{}
	for i := range 32 {
		channels = append(channels, compare(i))
	}
	again, err := h.comparisonChangeset([]changes.FileDiff{{Path: "f0.txt", Status: changes.StatusAdded, Modified: "content 0\n"}})
	if err != nil || again != channels[0] {
		t.Fatalf("identical comparison = %q, %v", again, err)
	}
	if _, err := h.comparisonChangeset([]changes.FileDiff{{Path: "extra.txt", Status: changes.StatusAdded, Modified: "x"}}); err == nil {
		t.Fatal("33rd fresh comparison was retained")
	} else {
		reviewWorkspaceChangesetCode(t, err, -32009)
	}
	result, sub, err := client.Subscribe(t.Context(), channels[0])
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	retained := result.Snapshot.State.Changeset.Files[0].Edit.After.Content.Uri
	h.mu.Lock()
	for i, channel := range channels {
		h.changesets[channel].touched = time.Now().Add(-time.Hour + time.Duration(i)*time.Second)
	}
	evicted := h.changesets[channels[1]].state.Files[0].Edit.After.Content.Uri
	h.mu.Unlock()
	compare(100)
	h.mu.Lock()
	_, kept := h.changesets[channels[0]]
	_, dropped := h.changesets[channels[1]]
	h.mu.Unlock()
	if !kept || dropped {
		t.Fatalf("eviction kept subscribed=%v, dropped oldest idle=%v", kept, !dropped)
	}
	var content types.ResourceReadResult
	err = client.Request(t.Context(), "resourceRead", map[string]any{"channel": types.RootResourceURI, "uri": evicted}, &content)
	if err == nil || !strings.Contains(err.Error(), "no longer retained") {
		t.Fatalf("evicted content read = %+v, %v", content, err)
	}
	for _, method := range []string{"resourceWrite", "resourceDelete"} {
		if err := client.Request(t.Context(), method, map[string]any{"channel": types.RootResourceURI, "uri": retained, "data": "x", "encoding": "utf-8"}, nil); err == nil {
			t.Fatalf("%s accepted changeset content", method)
		}
	}
}

func TestReviewWorkspaceChangesetConcurrentWritesAndRefreshes(t *testing.T) {
	root := reviewWorkspaceChangesetRepo(t, map[string]string{"a.txt": "base\n"})
	h := reviewWorkspaceChangesetHost(t, root)
	resource := func(method string, params map[string]any) (any, error) {
		params["channel"] = types.RootResourceURI
		raw, _ := json.Marshal(params)
		return h.resource(ahpRequest{Method: method, Params: raw})
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 40 {
			if _, err := resource("resourceWrite", map[string]any{"uri": h.fileURI("a.txt"), "data": fmt.Sprintf("edit %d\n", i), "encoding": "utf-8"}); err != nil {
				t.Error(err)
			}
		}
	})
	for _, channel := range []string{ahpWorkspaceChanges, ahpStagedChanges, ahpUnstagedChanges} {
		wg.Go(func() {
			for range 15 {
				if err := h.ensureChangeset(channel); err != nil {
					t.Error(err)
				}
				h.refreshChangesets()
			}
		})
	}
	wg.Go(func() {
		for range 40 {
			h.mu.Lock()
			uris := []string{}
			if c := h.changesets[ahpWorkspaceChanges]; c != nil {
				for _, file := range c.state.Files {
					if file.Edit.After != nil {
						uris = append(uris, file.Edit.After.Content.Uri)
					}
				}
			}
			h.mu.Unlock()
			for _, uri := range uris {
				if _, err := resource("resourceRead", map[string]any{"uri": uri}); err != nil && !strings.Contains(err.Error(), "no longer retained") {
					t.Error(err)
				}
			}
		}
	})
	wg.Go(func() {
		for range 6 {
			for _, op := range []struct{ channel, id string }{{ahpUnstagedChanges, "stage"}, {ahpStagedChanges, "unstage"}} {
				err := reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": op.channel, "operationId": op.id})
				var rpc *ahpRPCError
				if err != nil && (!errors.As(err, &rpc) || rpc.Code != -32602) {
					t.Error(err)
				}
			}
		}
	})
	wg.Wait()
	if t.Failed() {
		return
	}
	if err := reviewWorkspaceChangesetInvoke(h, map[string]any{"channel": ahpStagedChanges, "operationId": "unstage"}); err != nil && !strings.Contains(err.Error(), "not available") {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	file := reviewWorkspaceChangesetFile(t, h, ahpWorkspaceChanges, "a.txt")
	content, err := resource("resourceRead", map[string]any{"uri": file.Edit.After.Content.Uri})
	if err != nil || content.(types.ResourceReadResult).Data != string(disk) {
		t.Fatalf("after content = %+v, %v; disk %q", content, err, disk)
	}
}

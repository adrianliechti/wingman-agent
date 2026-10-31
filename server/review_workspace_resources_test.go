package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func reviewWorkspaceResourceHost(t *testing.T) (*Server, *ahpHost, string) {
	t.Helper()
	root := t.TempDir()
	app := newDebugTestServer(t, root)
	h := app.ahpHost()
	t.Cleanup(h.close)
	return app, h, root
}

func reviewWorkspaceResourceCall(h *ahpHost, method string, params map[string]any) (any, error) {
	params["channel"] = types.RootResourceURI
	raw, _ := json.Marshal(params)
	if method == "createResourceWatch" {
		return h.createResourceWatch(raw)
	}
	result, err := h.resource(ahpRequest{Method: method, Params: raw})
	return result, ahpResourceError(err)
}

func reviewWorkspaceResourceCode(t *testing.T, err error, want int) {
	t.Helper()
	var rpc *ahpRPCError
	if !errors.As(err, &rpc) || rpc.Code != want {
		t.Fatalf("want error %d, got %v", want, err)
	}
}

func reviewWorkspaceResourceWrite(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reviewWorkspaceResourceRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReviewWorkspaceResourceConfinement(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	reviewWorkspaceResourceWrite(t, outside, "secret", "secret")
	reviewWorkspaceResourceWrite(t, root, "a.txt", "inside")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	prefix := "file://" + filepath.ToSlash(root)
	base := filepath.Base(outside)
	outsideURIs := []string{
		prefix + "/../" + base + "/secret",
		prefix + "/%2e%2e/" + base + "/secret",
		prefix + "/sub%2F..%2F..%2F" + base + "%2Fsecret",
		prefix + "/./../" + base,
		prefix + "/..",
		"file://evil.example" + filepath.ToSlash(secret),
		"file:secret",
		"secret",
		"/" + filepath.ToSlash(secret),
		"FILE://" + filepath.ToSlash(secret),
		h.fileURI(secret),
		h.fileURI(outside),
		prefix + "/a.txt?x=1",
		prefix + "/a.txt#L1",
		"http://localhost" + filepath.ToSlash(root) + "/a.txt",
		"ahp-content:/" + strings.Repeat("0", 64),
		prefix + "/escape/secret",
		prefix + "/escape/new",
	}
	inside := h.fileURI("a.txt")
	for _, uri := range outsideURIs {
		for method, params := range map[string]map[string]any{
			"resourceRead":        {"uri": uri},
			"resourceResolve":     {"uri": uri},
			"resourceList":        {"uri": uri},
			"createResourceWatch": {"uri": uri, "recursive": true},
			"resourceWrite":       {"uri": uri, "data": "pwned", "encoding": "utf-8"},
			"resourceMkdir":       {"uri": uri},
			"resourceDelete":      {"uri": uri, "recursive": true},
			"resourceCopy":        {"source": inside, "destination": uri},
			"resourceMove":        {"source": uri, "destination": h.fileURI("moved")},
		} {
			if _, err := reviewWorkspaceResourceCall(h, method, params); err == nil {
				t.Errorf("%s accepted %s", method, uri)
			}
		}
		if _, err := reviewWorkspaceResourceCall(h, "resourceMove", map[string]any{"source": inside, "destination": uri}); err == nil {
			t.Fatalf("resourceMove moved a file to %s", uri)
		}
	}
	for _, uri := range []string{prefix + "/escape", prefix + "/leak.txt"} {
		for _, method := range []string{"resourceRead", "resourceList", "createResourceWatch"} {
			if _, err := reviewWorkspaceResourceCall(h, method, map[string]any{"uri": uri, "recursive": true}); err == nil {
				t.Errorf("%s followed %s outside the workspace", method, uri)
			}
		}
		if _, err := reviewWorkspaceResourceCall(h, "resourceResolve", map[string]any{"uri": uri}); err == nil {
			t.Errorf("resourceResolve followed %s outside the workspace", uri)
		}
		result, err := reviewWorkspaceResourceCall(h, "resourceResolve", map[string]any{"uri": uri, "followSymlinks": false})
		if err != nil {
			t.Fatal(err)
		}
		if info := result.(types.ResourceResolveResult); info.Type != types.ResourceTypeSymlink || info.Etag != nil || info.Uri != uri {
			t.Errorf("lstat of %s = %+v", uri, info)
		}
		if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": "pwned", "encoding": "utf-8"}); err == nil {
			t.Errorf("resourceWrite wrote through %s", uri)
		}
	}
	if runtime.GOOS != "windows" {
		uri := prefix + "/..%5C..%5C" + base + "%5Csecret"
		if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": "pwned", "encoding": "utf-8"}); err == nil {
			if _, err := os.Stat(filepath.Join(root, `..\..\`+base+`\secret`)); err != nil {
				t.Errorf("backslash name was not written inside the workspace: %v", err)
			}
		}
	}
	if got := reviewWorkspaceResourceRead(t, secret); got != "secret" {
		t.Fatalf("outside file changed: %q", got)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("outside directory changed: %v", entries)
	}
	if got := reviewWorkspaceResourceRead(t, filepath.Join(root, "a.txt")); got != "inside" {
		t.Fatalf("inside file changed: %q", got)
	}
}

// Bug: on a case-insensitive file system, renaming a.txt to A.txt is rejected as "source and destination refer to the same resource"; a case-only rename must succeed (VS Code treats it as a rename, not an overwrite).
func TestReviewWorkspaceResourceCaseOnlyRename(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	reviewWorkspaceResourceWrite(t, root, "readme.md", "content")
	if _, err := os.Stat(filepath.Join(root, "README.MD")); err != nil {
		t.Skip("case-sensitive file system")
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceMove", map[string]any{"source": h.fileURI("readme.md"), "destination": h.fileURI("README.md")}); err != nil {
		t.Fatalf("case-only rename rejected: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "README.md" {
		t.Fatalf("entries after rename = %v, %v", entries, err)
	}
}

// Bug: resourceMove without failIfExists fails on an existing non-empty directory although AHP says the destination is overwritten (resourceCopy does overwrite it).
func TestReviewWorkspaceResourceMoveOverwritesExistingDirectory(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	reviewWorkspaceResourceWrite(t, root, "a.txt", "moved")
	reviewWorkspaceResourceWrite(t, root, "copied/old.txt", "old")
	reviewWorkspaceResourceWrite(t, root, "target/old.txt", "old")
	if _, err := reviewWorkspaceResourceCall(h, "resourceCopy", map[string]any{"source": h.fileURI("a.txt"), "destination": h.fileURI("copied")}); err != nil {
		t.Fatal(err)
	}
	if got := reviewWorkspaceResourceRead(t, filepath.Join(root, "copied")); got != "moved" {
		t.Fatalf("copy did not overwrite: %q", got)
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceMove", map[string]any{"source": h.fileURI("a.txt"), "destination": h.fileURI("target")}); err != nil {
		t.Fatalf("move did not overwrite the existing directory: %v", err)
	}
	if got := reviewWorkspaceResourceRead(t, filepath.Join(root, "target")); got != "moved" {
		t.Fatalf("target = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("source still exists after move", err)
	}
}

// Bug: a recursive watch fails to start when any subdirectory is unreadable, so the workspace root cannot be watched at all; unreadable directories should be skipped.
func TestReviewWorkspaceResourceWatchSurvivesUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for the current user")
	}
	_, h, root := reviewWorkspaceResourceHost(t)
	reviewWorkspaceResourceWrite(t, root, "src/a.go", "package a")
	locked := filepath.Join(root, "data")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.directoryURI(), "recursive": true}); err != nil {
		t.Fatalf("workspace watch failed because of one unreadable directory: %v", err)
	}
}

func TestReviewWorkspaceResourceConcurrentAppendsAreAtomic(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	reviewWorkspaceResourceWrite(t, root, "log.txt", "")
	uri := h.fileURI("log.txt")
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": fmt.Sprintf("line %02d\n", i), "encoding": "utf-8", "mode": "append"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(reviewWorkspaceResourceRead(t, filepath.Join(root, "log.txt")), "\n"), "\n")
	slices.Sort(lines)
	if len(lines) != 64 || slices.Compact(lines)[63] != "line 63" {
		t.Fatalf("appends clobbered each other: %d lines", len(lines))
	}
}

func TestReviewWorkspaceResourceWriteModesAndEncodings(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	uri := h.fileURI("f.txt")
	path := filepath.Join(root, "f.txt")
	write := func(params map[string]any) error {
		params["uri"] = uri
		if _, ok := params["encoding"]; !ok {
			params["encoding"] = "utf-8"
		}
		_, err := reviewWorkspaceResourceCall(h, "resourceWrite", params)
		return err
	}
	reviewWorkspaceResourceCode(t, write(map[string]any{"data": "x", "ifMatch": fileRevision(nil)}), -32011)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("ifMatch on a missing file created it")
	}
	for _, step := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"data": "abcdef"}, "abcdef"},
		{map[string]any{"data": "XY", "position": 2}, "abXY"},
		{map[string]any{"data": "-", "mode": "append"}, "abXY-"},
		{map[string]any{"data": "+", "mode": "append", "position": 5}, "+abXY-"},
		{map[string]any{"data": "!", "mode": "insert", "position": 6}, "+abXY-!"},
		{map[string]any{"data": "", "position": 1}, "+"},
		{map[string]any{"data": "AP8=", "encoding": "base64", "mode": "append"}, "+\x00\xff"},
	} {
		if err := write(step.params); err != nil {
			t.Fatalf("%v: %v", step.params, err)
		}
		if got := reviewWorkspaceResourceRead(t, path); got != step.want {
			t.Fatalf("%v: got %q, want %q", step.params, got, step.want)
		}
	}
	for _, params := range []map[string]any{
		{"data": "x", "position": 4},
		{"data": "x", "mode": "append", "position": 4},
		{"data": "x", "mode": "insert", "position": -1},
		{"data": "x", "mode": "replace"},
		{"data": "x", "encoding": ""},
		{"data": "x", "encoding": "latin1"},
		{"data": "!!", "encoding": "base64"},
	} {
		reviewWorkspaceResourceCode(t, write(params), -32602)
	}
	reviewWorkspaceResourceCode(t, write(map[string]any{"data": "x", "createOnly": true}), -32010)
	reviewWorkspaceResourceCode(t, write(map[string]any{"data": "x", "ifMatch": fileRevision([]byte("stale"))}), -32011)
	if got := reviewWorkspaceResourceRead(t, path); got != "+\x00\xff" {
		t.Fatalf("rejected writes changed the file: %q", got)
	}
	read, err := reviewWorkspaceResourceCall(h, "resourceRead", map[string]any{"uri": uri, "encoding": "utf-8"})
	if err != nil {
		t.Fatal(err)
	}
	if content := read.(types.ResourceReadResult); content.Encoding != types.ContentEncodingBase64 || content.Data != base64.StdEncoding.EncodeToString([]byte("+\x00\xff")) {
		t.Fatalf("binary read = %+v", content)
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceRead", map[string]any{"uri": uri, "encoding": "latin1"}); err == nil {
		t.Fatal("unsupported read encoding accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": h.fileURI("dir"), "data": "x", "encoding": "utf-8"}); err == nil {
		t.Fatal("directory overwritten by resourceWrite")
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceRead", map[string]any{"uri": h.fileURI("dir")}); err == nil {
		t.Fatal("directory read as a file")
	}
}

func TestReviewWorkspaceResourceSizeLimits(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	uri := h.fileURI("big.txt")
	path := filepath.Join(root, "big.txt")
	exact := strings.Repeat("a", ahpResourceLimit)
	if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": exact, "encoding": "utf-8"}); err != nil {
		t.Fatal(err)
	}
	result, err := reviewWorkspaceResourceCall(h, "resourceResolve", map[string]any{"uri": uri})
	if err != nil {
		t.Fatal(err)
	}
	etag := result.(types.ResourceResolveResult).Etag
	if etag == nil || *etag != fileRevision([]byte(exact)) {
		t.Fatal("missing etag at the size limit")
	}
	reviewWorkspaceResourceCode(t, func() error {
		_, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": "b", "encoding": "utf-8", "mode": "append"})
		return err
	}(), -32602)
	reviewWorkspaceResourceCode(t, func() error {
		_, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": h.fileURI("other.txt"), "data": exact + "b", "encoding": "utf-8"})
		return err
	}(), -32602)
	if _, err := os.Stat(filepath.Join(root, "other.txt")); !os.IsNotExist(err) {
		t.Fatal("oversized write created a file")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != ahpResourceLimit {
		t.Fatalf("rejected append changed the file: %v, %v", info, err)
	}
	if err := os.WriteFile(path, []byte(exact+"b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceRead", map[string]any{"uri": uri}); err == nil {
		t.Fatal("oversized read accepted")
	}
	result, err = reviewWorkspaceResourceCall(h, "resourceResolve", map[string]any{"uri": uri})
	if err != nil {
		t.Fatal(err)
	}
	if info := result.(types.ResourceResolveResult); info.Etag != nil || info.Size == nil || *info.Size != ahpResourceLimit+1 {
		t.Fatalf("oversized resolve = %+v", info)
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": "c", "encoding": "utf-8", "ifMatch": *etag}); err == nil {
		t.Fatal("stale ifMatch accepted on an oversized file")
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": uri, "data": "small", "encoding": "utf-8"}); err != nil {
		t.Fatal(err)
	}
	if got := reviewWorkspaceResourceRead(t, path); got != "small" {
		t.Fatalf("overwrite of an oversized file = %d bytes", len(got))
	}
}

func TestReviewWorkspaceResourceResolveSymlinks(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	reviewWorkspaceResourceWrite(t, root, "dir/target.txt", "hello")
	if err := os.Symlink("dir/target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.txt", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	resolve := func(uri string, follow *bool) (types.ResourceResolveResult, error) {
		params := map[string]any{"uri": uri}
		if follow != nil {
			params["followSymlinks"] = *follow
		}
		result, err := reviewWorkspaceResourceCall(h, "resourceResolve", params)
		if err != nil {
			return types.ResourceResolveResult{}, err
		}
		return result.(types.ResourceResolveResult), nil
	}
	followed, err := resolve(h.fileURI("link.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if followed.Type != types.ResourceTypeFile || !strings.HasSuffix(followed.Uri, "/dir/target.txt") || followed.Etag == nil || *followed.Etag != fileRevision([]byte("hello")) {
		t.Fatalf("followed = %+v", followed)
	}
	if _, err := reviewWorkspaceResourceCall(h, "resourceRead", map[string]any{"uri": followed.Uri}); err != nil {
		t.Fatalf("canonical URI is not readable: %v", err)
	}
	link, err := resolve(h.fileURI("link.txt"), ahpPtr(false))
	if err != nil || link.Type != types.ResourceTypeSymlink || link.Uri != h.fileURI("link.txt") || link.Etag != nil {
		t.Fatalf("lstat = %+v, %v", link, err)
	}
	dir, err := resolve(h.fileURI("dir"), nil)
	if err != nil || dir.Type != types.ResourceTypeDirectory || dir.Etag != nil {
		t.Fatalf("directory = %+v, %v", dir, err)
	}
	_, err = resolve(h.fileURI("dangling"), nil)
	reviewWorkspaceResourceCode(t, err, int(types.ErrorCodeNotFound))
	if dangling, err := resolve(h.fileURI("dangling"), ahpPtr(false)); err != nil || dangling.Type != types.ResourceTypeSymlink {
		t.Fatalf("dangling lstat = %+v, %v", dangling, err)
	}
	_, err = resolve(h.fileURI("missing"), nil)
	reviewWorkspaceResourceCode(t, err, int(types.ErrorCodeNotFound))
}

func TestReviewWorkspaceResourceWatchPatternValidationAndLimit(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	items := func(patterns ...string) map[string]any { return map[string]any{"items": patterns} }
	many := make([]string, 129)
	for i := range many {
		many[i] = "x"
	}
	for _, patterns := range []any{
		items("/etc/**"), items("../**"), items("src/../../x"), items("["), items(strings.Repeat("a", 2049)), items(many...), "not an object",
	} {
		for _, field := range []string{"includes", "excludes"} {
			_, err := reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.fileURI("src"), field: patterns})
			reviewWorkspaceResourceCode(t, err, -32602)
		}
	}
	first, err := reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.fileURI("src"), "recursive": true})
	if err != nil {
		t.Fatal(err)
	}
	same, err := reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.fileURI("src"), "recursive": true})
	if err != nil || same != first {
		t.Fatalf("identical watch = %v, %v", same, err)
	}
	for i := 1; i < 64; i++ {
		if _, err := reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.fileURI("src"), "excludes": items(fmt.Sprint(i))}); err != nil {
			t.Fatal(i, err)
		}
	}
	_, err = reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.fileURI("src"), "excludes": items("overflow")})
	reviewWorkspaceResourceCode(t, err, -32602)
}

func TestReviewWorkspaceResourceWatchReportsFilteredBatches(t *testing.T) {
	app, h, root := reviewWorkspaceResourceHost(t)
	reviewWorkspaceResourceWrite(t, root, "src/keep.go", "package src")
	reviewWorkspaceResourceWrite(t, root, "src/deep/old.go", "package deep")
	reviewWorkspaceResourceWrite(t, root, "src/skip/old.go", "package skip")
	web := httptest.NewServer(app)
	t.Cleanup(web.Close)
	client := ahpTestClient(t, "ws"+strings.TrimPrefix(web.URL, "http")+"/ahp", "watch-batches")
	watch := func(params map[string]any) *ahp.Subscription {
		t.Helper()
		params["channel"] = types.RootResourceURI
		var created types.CreateResourceWatchResult
		if err := client.Request(t.Context(), "createResourceWatch", params, &created); err != nil {
			t.Fatal(err)
		}
		snapshot, sub, err := client.Subscribe(t.Context(), created.Channel)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sub.Close)
		if state := snapshot.Snapshot.State.ResourceWatch; state == nil || state.Root != params["uri"] {
			t.Fatalf("watch snapshot = %+v", snapshot.Snapshot)
		}
		return sub
	}
	recursive := watch(map[string]any{"uri": h.fileURI("src"), "recursive": true, "includes": map[string]any{"items": []string{"**/*.go"}}, "excludes": map[string]any{"items": []string{"skip"}}})
	shallow := watch(map[string]any{"uri": h.fileURI("src")})
	write := func(name, content string) {
		t.Helper()
		if _, err := reviewWorkspaceResourceCall(h, "resourceWrite", map[string]any{"uri": h.fileURI(name), "data": content, "encoding": "utf-8"}); err != nil {
			t.Fatal(err)
		}
	}
	write("src/new.go", "package src")
	write("src/notes.txt", "ignored by includes")
	write("src/deep/old.go", "package deep // changed")
	write("src/skip/old.go", "package skip // changed")
	if _, err := reviewWorkspaceResourceCall(h, "resourceDelete", map[string]any{"uri": h.fileURI("src/keep.go")}); err != nil {
		t.Fatal(err)
	}
	collect := func(sub *ahp.Subscription, until func(map[string]types.ResourceChangeType) bool) map[string]types.ResourceChangeType {
		t.Helper()
		seen := map[string]types.ResourceChangeType{}
		deadline := time.After(5 * time.Second)
		for {
			if until(seen) {
				// One more poll interval catches filtered paths reported in a later batch.
				deadline = time.After(1200 * time.Millisecond)
				until = func(map[string]types.ResourceChangeType) bool { return false }
			}
			select {
			case event := <-sub.Events():
				action, ok := event.(ahp.SubscriptionEventAction)
				if !ok {
					continue
				}
				changed, ok := action.Envelope.Action.Value.(*types.ResourceWatchChangedAction)
				if !ok {
					t.Fatalf("unexpected action %T", action.Envelope.Action.Value)
				}
				var batch struct {
					Items []types.ResourceChange `json:"items"`
				}
				if err := json.Unmarshal(changed.Changes, &batch); err != nil || len(batch.Items) == 0 {
					t.Fatalf("bad batch %s: %v", changed.Changes, err)
				}
				if !slices.IsSortedFunc(batch.Items, func(a, b types.ResourceChange) int { return strings.Compare(a.Uri, b.Uri) }) {
					t.Fatalf("unsorted batch %s", changed.Changes)
				}
				for _, item := range batch.Items {
					name := "."
					if item.Uri != h.fileURI("src") {
						name = strings.TrimPrefix(item.Uri, h.fileURI("src")+"/")
					}
					seen[name] = item.Type
				}
			case <-deadline:
				return seen
			}
		}
	}
	deep := collect(recursive, func(seen map[string]types.ResourceChangeType) bool {
		return seen["new.go"] != "" && seen["deep/old.go"] != "" && seen["keep.go"] != ""
	})
	want := map[string]types.ResourceChangeType{"new.go": types.ResourceChangeTypeAdded, "deep/old.go": types.ResourceChangeTypeUpdated, "keep.go": types.ResourceChangeTypeDeleted}
	for name, kind := range want {
		if deep[name] != kind {
			t.Fatalf("recursive watch saw %v, want %v", deep, want)
		}
	}
	for name := range deep {
		if _, ok := want[name]; !ok {
			t.Fatalf("recursive watch reported filtered %s", name)
		}
	}
	top := collect(shallow, func(seen map[string]types.ResourceChangeType) bool {
		return seen["new.go"] != "" && seen["notes.txt"] != "" && seen["keep.go"] != ""
	})
	for _, name := range []string{"new.go", "notes.txt", "keep.go"} {
		if _, ok := top[name]; !ok {
			t.Fatalf("shallow watch missed %s: %v", name, top)
		}
	}
	for name := range top {
		if strings.Contains(name, "/") {
			t.Fatalf("shallow watch reported nested %s", name)
		}
	}
}

func TestReviewWorkspaceResourceConcurrentMutationsUnderWatch(t *testing.T) {
	_, h, root := reviewWorkspaceResourceHost(t)
	if err := os.Mkdir(filepath.Join(root, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := reviewWorkspaceResourceCall(h, "createResourceWatch", map[string]any{"uri": h.fileURI("work"), "recursive": true})
	if err != nil {
		t.Fatal(err)
	}
	channel := string(result.(types.CreateResourceWatchResult).Channel)
	h.mu.Lock()
	scanner := h.watches[channel].scanner
	h.mu.Unlock()
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 25 {
				dir := fmt.Sprintf("work/w%d", worker)
				name := fmt.Sprintf("%s/f%d.txt", dir, i%3)
				calls := []struct {
					method string
					params map[string]any
				}{
					{"resourceMkdir", map[string]any{"uri": h.fileURI(dir)}},
					{"resourceWrite", map[string]any{"uri": h.fileURI(name), "data": fmt.Sprint(i), "encoding": "utf-8"}},
					{"resourceCopy", map[string]any{"source": h.fileURI(name), "destination": h.fileURI(name + ".copy")}},
					{"resourceMove", map[string]any{"source": h.fileURI(name + ".copy"), "destination": h.fileURI(name + ".moved")}},
					{"resourceRead", map[string]any{"uri": h.fileURI(name + ".moved")}},
					{"resourceResolve", map[string]any{"uri": h.fileURI(name)}},
					{"resourceList", map[string]any{"uri": h.fileURI(dir)}},
					{"resourceDelete", map[string]any{"uri": h.fileURI(name + ".moved")}},
				}
				for _, call := range calls {
					if _, err := reviewWorkspaceResourceCall(h, call.method, call.params); err != nil {
						t.Error(call.method, err)
					}
				}
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			if _, err := scanner.Changes(t.Context()); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Wait()
	if _, err := scanner.Changes(t.Context()); err != nil {
		t.Fatal(err)
	}
	for worker := range 4 {
		entries, err := os.ReadDir(filepath.Join(root, fmt.Sprintf("work/w%d", worker)))
		if err != nil || len(entries) != 3 {
			t.Fatalf("worker %d left %v, %v", worker, entries, err)
		}
	}
}

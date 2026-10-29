package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestAHPResourcesPreserveFileSemantics(test *testing.T) {
	root := test.TempDir()
	app := newDebugTestServer(test, root)
	h := app.ahpHost()
	call := func(method string, params map[string]any) (any, error) {
		params["channel"] = t.RootResourceURI
		raw, _ := json.Marshal(params)
		result, err := h.resource(ahpRequest{Method: method, Params: raw})
		return result, ahpResourceError(err)
	}
	must := func(method string, params map[string]any) any {
		test.Helper()
		v, err := call(method, params)
		if err != nil {
			test.Fatal(method, err)
		}
		return v
	}
	code := func(method string, params map[string]any, want int) {
		test.Helper()
		_, err := call(method, params)
		var rpc *ahpRPCError
		if !errors.As(err, &rpc) || rpc.Code != want {
			test.Fatalf("%s: want %d, got %v", method, want, err)
		}
	}
	dir := h.fileURI("nested/child")
	must("resourceMkdir", map[string]any{"uri": dir})
	must("resourceMkdir", map[string]any{"uri": dir})
	uri := h.fileURI("nested/child/a 🌍.txt")
	must("resourceWrite", map[string]any{"uri": uri, "data": "hello 🌍", "encoding": "utf-8", "createOnly": true})
	code("resourceWrite", map[string]any{"uri": uri, "data": "clobber", "encoding": "utf-8", "createOnly": true}, -32010)
	info := must("resourceResolve", map[string]any{"uri": uri}).(t.ResourceResolveResult)
	if info.Etag == nil || *info.Etag != fileRevision([]byte("hello 🌍")) {
		test.Fatal(info)
	}
	var succeeded atomic.Int32
	var wg sync.WaitGroup
	for _, text := range []string{"first", "second"} {
		wg.Go(func() {
			_, err := call("resourceWrite", map[string]any{"uri": uri, "data": text, "encoding": "utf-8", "ifMatch": *info.Etag})
			if err == nil {
				succeeded.Add(1)
			} else {
				var rpc *ahpRPCError
				if !errors.As(err, &rpc) || rpc.Code != -32011 {
					test.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if succeeded.Load() != 1 {
		test.Fatal("concurrent stale save was accepted")
	}
	must("resourceWrite", map[string]any{"uri": uri, "data": "abc", "encoding": "utf-8"})
	must("resourceWrite", map[string]any{"uri": uri, "data": "!", "encoding": "utf-8", "mode": "append", "position": 1})
	must("resourceWrite", map[string]any{"uri": uri, "data": "?", "encoding": "utf-8", "mode": "insert", "position": 1})
	content := must("resourceRead", map[string]any{"uri": uri}).(t.ResourceReadResult)
	if content.Data != "a?b!c" {
		test.Fatal(content)
	}
	copyURI := h.fileURI("copy.txt")
	must("resourceCopy", map[string]any{"source": uri, "destination": copyURI, "failIfExists": true})
	code("resourceCopy", map[string]any{"source": uri, "destination": copyURI, "failIfExists": true}, -32010)
	movedURI := h.fileURI("moved.txt")
	must("resourceMove", map[string]any{"source": copyURI, "destination": movedURI, "failIfExists": true})
	code("resourceRead", map[string]any{"uri": copyURI}, -32008)
	binaryURI := h.fileURI("binary.bin")
	must("resourceWrite", map[string]any{"uri": binaryURI, "data": "AP8BAg==", "encoding": "base64"})
	binary := must("resourceRead", map[string]any{"uri": binaryURI, "encoding": "utf-8"}).(t.ResourceReadResult)
	if binary.Encoding != t.ContentEncodingBase64 || binary.Data != "AP8BAg==" {
		test.Fatal(binary)
	}
	must("resourceDelete", map[string]any{"uri": dir, "recursive": true})
	code("resourceRead", map[string]any{"uri": uri}, -32008)
	code("resourceDelete", map[string]any{"uri": dir, "recursive": true}, -32008)
	for _, method := range []string{"resourceWrite", "resourceDelete", "resourceMkdir"} {
		if _, err := call(method, map[string]any{"uri": h.directoryURI(), "data": "bad", "encoding": "utf-8", "recursive": true}); err == nil {
			test.Fatal("workspace root mutated", method)
		}
	}
	outside := test.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		test.Fatal(err)
	}
	for _, method := range []string{"resourceRead", "resourceWrite", "resourceDelete"} {
		if _, err := call(method, map[string]any{"uri": h.fileURI("escape/secret"), "data": "bad", "encoding": "utf-8"}); err == nil {
			test.Fatal("escaped workspace", method)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "secret")); string(data) != "secret" {
		test.Fatal("outside file changed")
	}
	must("resourceMkdir", map[string]any{"uri": h.fileURI("parent/child")})
	if err := os.Symlink("parent/child", filepath.Join(root, "alias")); err != nil {
		test.Fatal(err)
	}
	if _, err := call("resourceCopy", map[string]any{"source": h.fileURI("alias"), "destination": h.fileURI("parent")}); err == nil {
		test.Fatal("copy over ancestor accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "parent/child")); err != nil {
		test.Fatal("ancestor copy removed source", err)
	}
}

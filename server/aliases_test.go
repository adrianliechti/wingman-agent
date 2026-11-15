package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func aliasTestProject(t *testing.T) (*Project, string) {
	t.Helper()
	t.Setenv("WINGMAN_HOME", t.TempDir())
	dir := t.TempDir()
	h := newHost(t.Context())
	ctx, cancel := context.WithCancel(h.ctx)
	p := &Project{host: h, dir: dir, scope: workspaceScope(dir), ctx: ctx, cancel: cancel}
	t.Cleanup(h.Close)
	t.Cleanup(p.close)
	aliases, err := p.aliasDirectory()
	if err != nil {
		t.Fatal(err)
	}
	return p, aliases
}

func readTestAlias(t *testing.T, name string) sessionAlias {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var alias sessionAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		t.Fatal(err)
	}
	return alias
}

func TestAliasMetadataRetriesUnchangedValueAtClose(t *testing.T) {
	p, dir := aliasTestProject(t)
	alias := sessionAlias{Resource: "ahp-session:/retry", Backend: "one", Session: "native", Title: "Before"}
	if err := p.storeAlias(alias); err != nil {
		t.Fatal(err)
	}
	name := aliasPath(dir, alias)
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	// An existing directory prevents the atomic file replacement on all platforms.
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	a := &Session{project: p, resource: alias.Resource, state: types.SessionState{Title: "After"}}
	p.host.mu.Lock()
	a.keep(types.SessionSummary{})
	p.host.mu.Unlock()
	if err := p.flushAliases(); err == nil {
		t.Fatal("metadata write unexpectedly succeeded")
	}
	// Publishing the same value does not stage another update. The failed write
	// must remain pending even though the in-memory alias already has that value.
	p.host.mu.Lock()
	a.keep(types.SessionSummary{})
	p.host.mu.Unlock()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	p.close()
	if got := readTestAlias(t, name); got.Title != "After" {
		t.Fatalf("retry lost metadata: %+v", got)
	}
}

func TestAliasDeletionRetriesAndAllowsURIReuse(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprint(reuse), func(t *testing.T) {
			p, dir := aliasTestProject(t)
			alias := sessionAlias{Resource: "ahp-session:/reused", Backend: "one", Session: "old"}
			if err := p.storeAlias(alias); err != nil {
				t.Fatal(err)
			}
			name := aliasPath(dir, alias)
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(name, 0700); err != nil {
				t.Fatal(err)
			}
			blocker := filepath.Join(name, "blocker")
			if err := os.WriteFile(blocker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			p.host.mu.Lock()
			p.forgetAlias(alias.Resource)
			p.host.mu.Unlock()
			if err := p.flushAliases(); err == nil {
				t.Fatal("deletion unexpectedly succeeded")
			}
			if err := os.Remove(blocker); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if reuse {
				alias.Session = "replacement"
				if err := p.storeAlias(alias); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.flushAliases(); err != nil {
				t.Fatal(err)
			}
			if reuse {
				if got := readTestAlias(t, name); got.Session != "replacement" {
					t.Fatalf("stale deletion replaced the new alias: %+v", got)
				}
			} else if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("deleted alias reappeared: %v", err)
			}
			p.host.mu.Lock()
			pending := len(p.aliases.pending)
			p.host.mu.Unlock()
			if pending != 0 {
				t.Fatalf("successful retry left %d pending changes", pending)
			}
		})
	}
}

func TestAliasConcurrentUpdatesAndDeletionKeepLatestValue(t *testing.T) {
	p, dir := aliasTestProject(t)
	alias := sessionAlias{Resource: "ahp-session:/concurrent", Chat: "ahp-chat:/reused", Backend: "one"}
	var workers sync.WaitGroup
	for i := range 40 {
		workers.Go(func() {
			change := alias
			change.Session = fmt.Sprint(i)
			p.host.mu.Lock()
			p.host.rememberAliasLocked(change)
			p.stageAliasLocked(change, false)
			if i%2 == 0 {
				p.forgetChatAlias(change.Chat)
			}
			p.host.mu.Unlock()
			if err := p.flushAliases(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	// Every completed flush must reflect the latest publication, regardless of
	// whether a write or deletion was already running when it was staged.
	p.host.mu.Lock()
	want, exists := p.host.chatAliases[alias.Chat]
	p.host.mu.Unlock()
	name := aliasPath(dir, alias)
	if exists {
		if got := readTestAlias(t, name); got != want {
			t.Fatalf("saved alias = %+v, published alias = %+v", got, want)
		}
	} else if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted alias reappeared: %v", err)
	}
}

func TestAliasFailureDoesNotFailAnotherSessionCreation(t *testing.T) {
	p, dir := aliasTestProject(t)
	broken := sessionAlias{Resource: "ahp-session:/broken", Backend: "one", Session: "broken"}
	if err := os.MkdirAll(aliasPath(dir, broken), 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.storeAlias(broken); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	created := sessionAlias{Resource: "ahp-session:/created", Backend: "one", Session: "created"}
	if err := p.storeAlias(created); err != nil {
		t.Fatalf("unrelated metadata failure prevented creation: %v", err)
	}
	if got := readTestAlias(t, aliasPath(dir, created)); got.Session != created.Session {
		t.Fatalf("created alias = %+v", got)
	}
	if err := os.Remove(aliasPath(dir, broken)); err != nil {
		t.Fatal(err)
	}
}

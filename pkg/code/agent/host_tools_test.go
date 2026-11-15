package agent

import (
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestHostToolsJoinTheSessionNotItsSubagents(t *testing.T) {
	a := New(newOptionsTestWorkspace(t), Config{}, nil)
	defer a.Close()
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetHostTools("missing", func() []tool.Tool { return nil }); err == nil {
		t.Fatal("an unknown session took tools")
	}
	lent := []tool.Tool{{Name: "read", Description: "the host's reader"}, {Name: "host_only", Description: "only the host has it"}}
	if err := a.SetHostTools(id, func() []tool.Tool { return lent }); err != nil {
		t.Fatal(err)
	}
	s := a.session(id)
	described := func(tools []tool.Tool, name string) (found []string) {
		for _, t := range tools {
			if t.Name == name {
				found = append(found, t.Description)
			}
		}
		return found
	}
	own := s.catalogTools()
	if reads := described(own, "read"); len(reads) != 1 || reads[0] == "the host's reader" || len(described(own, "host_only")) != 0 {
		t.Fatalf("the host's tools reached the catalogue that subagents share: %q", reads)
	}
	tools := s.sessionTools()
	if reads := described(tools, "read"); !slices.Equal(reads, []string{"the host's reader"}) {
		t.Fatalf("read tools of the session: %q", reads)
	}
	if len(described(tools, "host_only")) != 1 || len(tools) != len(own)+1 {
		t.Fatalf("session has %d tools, its catalogue %d", len(tools), len(own))
	}
}

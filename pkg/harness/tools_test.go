package harness

import (
	"context"
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestPinnedCatalogRetainsLivePolicy(t *testing.T) {
	catalog := []tool.Tool{{Name: "read"}, {Name: "edit"}}
	readOnly := false
	a := &Agent{Config: &Config{
		PinTools: true,
		Tools:    func() []tool.Tool { return catalog },
		FilterTools: func(tools []tool.Tool) []tool.Tool {
			if readOnly {
				return slices.DeleteFunc(tools, func(t tool.Tool) bool { return t.Name == "edit" })
			}
			return tools
		},
	}}
	stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog[0].Name = "replacement"
	catalog = nil
	if tools := a.AvailableTools(); len(tools) != 2 || tools[0].Name != "read" {
		t.Fatalf("catalog changed during the turn: %v", tools)
	}
	readOnly = true
	if tools := a.AvailableTools(); len(tools) != 1 || tools[0].Name != "read" {
		t.Fatalf("policy did not change during the turn: %v", tools)
	}
	readOnly = false
	if tools := a.AvailableTools(); len(tools) != 2 || tools[1].Name != "edit" {
		t.Fatalf("filter mutated the pinned catalog: %v", tools)
	}
	a.Cancel(nil)
	for range stream {
	}
	if tools := a.AvailableTools(); len(tools) != 0 {
		t.Fatalf("catalog stayed pinned after the turn: %v", tools)
	}
}

func TestEmptyCatalogIsPinnedAfterPreparation(t *testing.T) {
	catalog := []tool.Tool{{Name: "before_preparation"}}
	a := &Agent{Config: &Config{
		PinTools: true,
		Tools:    func() []tool.Tool { return catalog },
	}, PrepareTurn: func(_ context.Context, input []Content) ([]Content, error) {
		catalog = nil
		return input, nil
	}}
	stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog = []tool.Tool{{Name: "arrived_mid_turn"}}
	if tools := a.AvailableTools(); len(tools) != 0 {
		t.Fatalf("empty catalog was not captured after preparation: %v", tools)
	}
	a.Cancel(nil)
	for range stream {
	}
	if tools := a.AvailableTools(); len(tools) != 1 || tools[0].Name != "arrived_mid_turn" {
		t.Fatalf("catalog did not refresh after the turn: %v", tools)
	}
}

func TestContextStatsIncludesToolsAddedByPolicy(t *testing.T) {
	a := &Agent{Config: &Config{FilterTools: func(tools []tool.Tool) []tool.Tool {
		return append(tools, tool.Tool{Name: "report", Description: "Report a structured result"})
	}}}
	stats := a.ContextStats()
	if stats.ToolsTokens == 0 || len(stats.ToolStats) != 1 || stats.ToolStats[0].Name != "report" {
		t.Fatalf("policy-provided tools missing from context stats: %+v", stats)
	}
}

package prompt

import (
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/model"
)

// catalogRouting records the models/ directory each Claude and GPT catalog
// model must use. Routing is by longest prefix, so a new minor version would
// otherwise inherit an older sibling's prompt, or the family default, without
// any failure: claude-fable-5-2 used Fable 5's prompt and gpt-6.1-terra the
// generic GPT one until each got a directory. Add new catalog models here
// with a deliberate choice.
var catalogRouting = map[string]string{
	"claude-sonnet-5-5": "claude-sonnet-5-5",
	"claude-haiku-5-5":  "claude-haiku-5-5",
	"claude-sonnet-5":   "claude",
	"claude-opus-5-5":   "claude-opus-5-5",
	"claude-opus-5":     "claude-opus-5",
	"claude-opus-4-8":   "claude-opus-4-8",
	"claude-opus-4-7":   "claude",
	"claude-sonnet-4-6": "claude",
	"claude-opus-4-6":   "claude",
	"claude-haiku-4-6":  "claude",
	"claude-sonnet-4-5": "claude",
	"claude-opus-4-5":   "claude",
	"claude-haiku-4-5":  "claude",
	"claude-fable-5-2":  "claude-fable-5-2",
	"claude-fable-5-1":  "claude-fable-5-1",
	"claude-fable-5":    "claude-fable-5",
	"claude-mythos-5-1": "claude-mythos-5-1",
	"claude-mythos-5":   "claude-mythos-5",

	"gpt-6-astra":         "gpt-6-astra",
	"gpt-6.1-sol":         "gpt-6-1-sol",
	"gpt-6.1-terra":       "gpt-6-1-terra",
	"gpt-6-sol":           "gpt-6-sol",
	"gpt-6-luna":          "gpt-6-luna",
	"gpt-5.6-sol":         "gpt",
	"gpt-5.6-terra":       "gpt",
	"gpt-5.6-luna":        "gpt",
	"gpt-5.5":             "gpt-5-5",
	"gpt-5.4":             "gpt-5-4",
	"gpt-5.4-mini":        "gpt-5-4-mini",
	"gpt-5.3-codex":       "gpt",
	"gpt-5.3-codex-spark": "gpt",
	"gpt-5.2-codex":       "gpt-5-2",
	"gpt-5.2":             "gpt-5-2",
	"gpt-5.1-codex-max":   "gpt-5-1",
	"gpt-5.1-codex":       "gpt-5-1",
	"gpt-5.1-codex-mini":  "gpt-5-1",
	"gpt-5.1":             "gpt-5-1",
	"gpt-5-codex":         "gpt",
	"gpt-5":               "gpt",
	"gpt-5-mini":          "gpt",
}

func TestCatalogModelRouting(t *testing.T) {
	listed := map[string]bool{}

	for _, selected := range model.Models {
		got := variantKey(selected.ID)

		if selected.Namespace != "anthropic" && selected.Namespace != "openai" {
			if got != "" {
				t.Errorf("%s/%s uses models/%s, want the shared fallback", selected.Namespace, selected.ID, got)
			}
			continue
		}

		want, ok := catalogRouting[selected.ID]
		if !ok {
			t.Errorf("%s has no routing decision (it currently uses %q); add it to catalogRouting", selected.ID, got)
			continue
		}
		listed[selected.ID] = true

		if _, ok := variants[want]; !ok {
			t.Errorf("%s is routed to models/%s, which does not exist", selected.ID, want)
			continue
		}
		if got != want {
			t.Errorf("%s uses models/%s, want models/%s", selected.ID, got, want)
		}
		for _, alias := range append([]string{selected.Namespace + "/" + selected.ID, selected.ID + ":latest"}, selected.Aliases...) {
			if aliased := variantKey(alias); aliased != want {
				t.Errorf("%s (alias of %s) uses models/%s, want models/%s", alias, selected.ID, aliased, want)
			}
		}
	}

	for id := range catalogRouting {
		if !listed[id] {
			t.Errorf("catalogRouting lists %s, which is not a Claude or GPT catalog model", id)
		}
	}
}

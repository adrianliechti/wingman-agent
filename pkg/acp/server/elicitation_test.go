package server

import (
	"reflect"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestElicitationSchemaPreservesEnumDescriptions(t *testing.T) {
	schema := elicitationSchema(tool.ElicitRequest{Fields: []tool.ElicitField{{
		Name:             "strategy",
		Type:             "string",
		Title:            "Strategy",
		Description:      "Choose a strategy",
		Required:         true,
		Enum:             []string{"safe", "fast"},
		EnumDescriptions: []string{"Minimal changes", "Broader optimization"},
		Default:          "safe",
	}}})

	property, ok := schema.Properties["strategy"].(map[string]any)
	if !ok {
		t.Fatalf("property = %#v", schema.Properties["strategy"])
	}
	choices, ok := property["oneOf"].([]map[string]any)
	if !ok || len(choices) != 2 {
		t.Fatalf("oneOf = %#v", property["oneOf"])
	}
	if choices[0]["const"] != "safe" || choices[0]["description"] != "Minimal changes" {
		t.Fatalf("first choice = %#v", choices[0])
	}
	if !reflect.DeepEqual(schema.Required, []string{"strategy"}) {
		t.Fatalf("required = %v", schema.Required)
	}
}

func TestElicitationSchemaUsesAnyOfForMultiSelect(t *testing.T) {
	schema := elicitationSchema(tool.ElicitRequest{Fields: []tool.ElicitField{{
		Name:             "targets",
		Type:             "string",
		Multiple:         true,
		Enum:             []string{"tests", "docs"},
		EnumDescriptions: []string{"Run tests", "Update docs"},
	}}})

	property := schema.Properties["targets"].(map[string]any)
	if property["type"] != "array" {
		t.Fatalf("type = %v", property["type"])
	}
	items := property["items"].(map[string]any)
	if _, ok := items["anyOf"].([]map[string]any); !ok {
		t.Fatalf("items = %#v", items)
	}
}

func TestElicitationFallbackUsesDefaults(t *testing.T) {
	result := elicitationFallback(tool.ElicitRequest{Fields: []tool.ElicitField{
		{Name: "mode", Required: true, Default: "safe"},
		{Name: "note"},
	}})
	if result.Action != tool.ElicitAccept || result.Content["mode"] != "safe" {
		t.Fatalf("result = %#v", result)
	}

	result = elicitationFallback(tool.ElicitRequest{Fields: []tool.ElicitField{{Name: "name", Required: true}}})
	if result.Action != tool.ElicitCancel {
		t.Fatalf("missing required value should cancel, got %#v", result)
	}
}

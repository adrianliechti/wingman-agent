package server

import (
	"encoding/json"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/google/jsonschema-go/jsonschema"
)

func TestElicitationSchemaAllowsOtherOnlyForAdvisoryChoices(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, multiple := range []bool{false, true} {
			wire := elicitationSchema(tool.ElicitRequest{Fields: []tool.ElicitField{{
				Name: "choice", Type: "string", Required: true, Strict: strict, Multiple: multiple,
				Enum: []string{"A", "B"}, EnumDescriptions: []string{"First choice", "Second choice"},
			}}})
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			var listed, other any = "A", "Custom answer"
			if multiple {
				listed = []any{"A", "B"}
				other = []any{"A", "Custom answer"}
			}
			if err := resolved.Validate(map[string]any{"choice": listed}); err != nil {
				t.Fatalf("listed choice rejected: %v", err)
			}
			err = resolved.Validate(map[string]any{"choice": other})
			if (err != nil) != strict {
				t.Fatalf("strict=%v multiple=%v: custom answer error=%v", strict, multiple, err)
			}
			if err := resolved.Validate(map[string]any{}); err == nil {
				t.Fatal("required choice no longer enforced")
			}
		}
	}
}

package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestElicitationSchemaAllowsOtherOnlyForAdvisoryChoices(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, multiple := range []bool{false, true} {
			wire := testElicitationSchema(t, tool.ElicitRequest{Fields: []tool.ElicitField{{
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
			if multiple && !strict {
				input := map[string]any{"choice": []any{"A"}, "choice_custom": "Custom answer"}
				if err := resolved.Validate(input); err != nil {
					t.Fatal(err)
				}
				result := elicitationAnswers(tool.ElicitRequest{Fields: []tool.ElicitField{{Name: "choice", Multiple: true, Enum: []string{"A", "B"}, Required: true}}}, input)
				if !reflect.DeepEqual(result.Content["choice"], other) || len(result.Content) != 1 {
					t.Fatalf("combined answer = %#v", result)
				}
				if missing := elicitationAnswers(tool.ElicitRequest{Fields: []tool.ElicitField{{Name: "choice", Required: true}}}, nil); missing.Action != tool.ElicitCancel {
					t.Fatal("accepted a missing required answer")
				}
				continue
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

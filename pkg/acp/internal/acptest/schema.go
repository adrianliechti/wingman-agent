package acptest

import (
	_ "embed"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

//go:embed testdata/v2-schema.json
var v2Schema []byte

var resolvedV2Schemas sync.Map

func ValidateV2(t *testing.T, name string, value any) {
	t.Helper()
	resolved, ok := resolvedV2Schemas.Load(name)
	if !ok {
		var schema jsonschema.Schema
		if err := json.Unmarshal(v2Schema, &schema); err != nil {
			t.Fatal(err)
		}
		schema.Ref = "#/$defs/" + name
		r, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		resolved, _ = resolvedV2Schemas.LoadOrStore(name, r)
	}
	if err := resolved.(*jsonschema.Resolved).Validate(value); err != nil {
		t.Fatalf("invalid ACP v2 %s: %v\n%v", name, err, value)
	}
}

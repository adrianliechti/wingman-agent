// Package acpjson converts between ACP's JSON values and native backend payloads.
package acpjson

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/coder/acp-go-sdk"
)

// Metadata encodes optional, backend-supplied JSON metadata.
func Metadata(fields map[string]any) acp.Meta {
	meta, _ := acp.NewMeta(fields)
	return meta
}

// Object reads a JSON object without rounding integer IDs or token counts.
func Object(value any) map[string]any {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&fields) != nil {
		return nil
	}
	return fields
}

// Raw encodes a JSON-compatible backend value for an optional protocol field.
func Raw(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	raw, _ := json.Marshal(value)
	return raw
}

// Properties converts the built-in tool's dynamic form fields to the SDK union.
func Properties(fields map[string]any) (map[string]acp.ElicitationPropertySchema, error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode elicitation fields: %w", err)
	}
	var properties map[string]acp.ElicitationPropertySchema
	if err := json.Unmarshal(data, &properties); err != nil {
		return nil, fmt.Errorf("invalid elicitation fields: %w", err)
	}
	return properties, nil
}

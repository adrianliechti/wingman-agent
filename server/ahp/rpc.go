// Package ahp implements the wire conventions shared by the host's transports.
// It has no dependency on projects, agents or the web UI.
package ahp

import (
	"encoding/json"
	"errors"
	"os"

	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string           { return e.Message }
func InvalidParams(message string) error { return &Error{Code: -32602, Message: message} }

// Decode distinguishes invalid JSON from an invalid JSON-RPC request.
func Decode(data []byte) (Request, error) {
	var req Request
	if json.Unmarshal(data, &req) != nil {
		if json.Valid(data) {
			return Request{}, &Error{Code: -32600, Message: "invalid JSON-RPC request"}
		}
		return Request{}, &Error{Code: -32700, Message: "invalid JSON"}
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return req, &Error{Code: -32600, Message: "invalid JSON-RPC request"}
	}
	return req, nil
}

func Reply(id json.RawMessage, result any, err error) any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if err != nil {
		var rpc *Error
		if !errors.As(err, &rpc) {
			rpc = &Error{Code: -32603, Message: err.Error()}
			if os.IsNotExist(err) {
				rpc.Code = int(types.ErrorCodeNotFound)
			} else if os.IsPermission(err) {
				rpc.Code = int(types.ErrorCodePermissionDenied)
			}
		}
		return struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *Error          `json:"error"`
		}{"2.0", id, rpc}
	}
	return struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result}
}

// Notification is an unsolicited JSON-RPC message.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func Notify(method string, params any) Notification {
	return Notification{JSONRPC: "2.0", Method: method, Params: params}
}

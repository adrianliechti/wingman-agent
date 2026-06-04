package jsonrpc2

import (
	"encoding/json"
)

var (
	ErrParse = NewError(-32700, "parse error")

	ErrInvalidRequest = NewError(-32600, "invalid request")

	ErrMethodNotFound = NewError(-32601, "method not found")

	ErrInvalidParams = NewError(-32602, "invalid params")

	ErrInternal = NewError(-32603, "internal error")

	ErrServerOverloaded = NewError(-32000, "overloaded")

	ErrUnknown = NewError(-32001, "unknown error")

	ErrServerClosing = NewError(-32004, "server is closing")

	ErrClientClosing = NewError(-32003, "client is closing")

	ErrRejected = NewError(-32005, "rejected by transport")
)

const wireVersion = "2.0"

type wireCombined struct {
	VersionTag string          `json:"jsonrpc"`
	ID         any             `json:"id,omitempty"`
	Method     string          `json:"method,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *WireError      `json:"error,omitempty"`
}

type WireError struct {
	Code int64 `json:"code"`

	Message string `json:"message"`

	Data json.RawMessage `json:"data,omitempty"`
}

func NewError(code int64, message string) error {
	return &WireError{
		Code:    code,
		Message: message,
	}
}

func (err *WireError) Error() string {
	return err.Message
}

func (err *WireError) Is(other error) bool {
	w, ok := other.(*WireError)
	if !ok {
		return false
	}
	return err.Code == w.Code
}

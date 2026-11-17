package acp

import acpsdk "github.com/coder/acp-go-sdk"

// CodeResourceNotFound is ACP's error for a resource an agent does not have.
const CodeResourceNotFound = -32002

// SessionNotFound answers a request for a session the agent does not have,
// such as one never prompted or whose history is gone, as the reference
// adapters do: "resource not found" with the session's id.
func SessionNotFound(id acpsdk.SessionId) *acpsdk.RequestError {
	return &acpsdk.RequestError{Code: CodeResourceNotFound, Message: "Resource not found", Data: map[string]any{"sessionId": id}}
}

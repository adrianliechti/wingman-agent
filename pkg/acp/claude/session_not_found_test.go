package claude

import (
	"errors"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

// A session never prompted has no transcript; resuming it is "resource not
// found", as the reference adapter answers, so clients can tell it is gone.
func TestResumingASessionWithoutTranscriptIsNotFound(t *testing.T) {
	a := &Agent{env: []string{"CLAUDE_CONFIG_DIR=" + t.TempDir()}}
	_, err := a.resumeSourceFor(t.TempDir(), acpsdk.SessionId(uuid.NewString()))
	var request *acpsdk.RequestError
	if !errors.As(err, &request) || request.Code != acp.CodeResourceNotFound {
		t.Fatalf("resuming a session without transcript: %v", err)
	}
}

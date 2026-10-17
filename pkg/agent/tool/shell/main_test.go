package shell

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Process/session tests select the unconfined runner explicitly. Sandbox
	// policy and enforcement tests opt back in and exercise the real backend.
	os.Setenv("WINGMAN_SANDBOX", "off")
	os.Exit(m.Run())
}

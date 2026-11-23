package server

import (
	"os"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func TestMain(m *testing.M) {
	// Installed language servers must not change results; a test that wants one asks for it.
	lsp.CommandLookup = func(string) string { return "" }
	os.Exit(testenv.RunWithWingmanHome(m.Run))
}

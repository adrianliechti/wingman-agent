package pi

import (
	"os"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

func TestMain(m *testing.M) {
	os.Exit(acptest.Main(m))
}

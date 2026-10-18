package codex

import (
	"encoding/json"
	"os"
	"runtime"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

func TestMain(m *testing.M) {
	os.Exit(acptest.Main(m))
}

// Session roots must be absolute on the current platform, so fake sessions
// use /work on Unix and C:\work on Windows.
var (
	workCwd         = testAbsPath("work")
	contractCwd     = testAbsPath("contract")
	contractCwdJSON = jsonString(contractCwd)
)

func testAbsPath(name string) string {
	if runtime.GOOS == "windows" {
		return `C:\` + name
	}
	return "/" + name
}

func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

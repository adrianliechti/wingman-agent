package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/graph"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func TestLiveGraphInterfaceDispatch(t *testing.T) {
	if os.Getenv("WINGMAN_LIVE_LSP_AGENT") == "" {
		t.Skip("set WINGMAN_LIVE_LSP_AGENT=1; requires an installed managed gopls")
	}
	managed, err := devtools.New()
	if err != nil {
		t.Fatal(err)
	}
	if managed.Resolve("gopls") == "" {
		t.Fatal("installed managed gopls required; this test never installs tools")
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	root := writeFixture(t, fixture{files: map[string]string{
		"go.mod":     "module codeintel-fixture.test\n\ngo 1.26\n",
		"p1/use.go":  "package p1\ntype Runner interface {\n Run() error\n}\nfunc Use(r Runner) error { return r.Run() }\n",
		"p2/impl.go": "package p2\ntype Impl struct{}\nfunc (*Impl) Run() error { return nil }\n",
	}})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for _, online := range []bool{false, true} {
		name := "without_lsp"
		if online {
			name = "with_lsp"
		}
		t.Run(name, func(t *testing.T) {
			var options []lsp.ManagerOption
			if online {
				options = append(options, lsp.WithManagedTools(managed))
			}
			service := language.New(root, filepath.Join(t.TempDir(), "graph.json"), options...)
			defer service.Close()
			result, err := service.Graph().Neighborhood(ctx, "", "Run", "p2/")
			if err != nil {
				t.Fatal(err)
			}
			if online {
				if len(result.Callers) != 1 || result.Callers[0].Name != "Use" || service.Graph().EdgeStats()[graph.ViaLSP] == 0 {
					t.Fatalf("implementation caller missing: %+v", result)
				}
			} else if len(result.Callers) != 0 || len(service.Manager().Activities()) != 0 {
				t.Fatalf("offline graph invented an implementation or started a server: %+v", result)
			}
		})
	}
}

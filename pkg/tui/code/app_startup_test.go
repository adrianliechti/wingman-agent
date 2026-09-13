package code

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

type blockedModelAgent struct {
	*uiTestAgent
	started chan struct{}
	stopped chan struct{}
}

func (a *blockedModelAgent) FetchModels(ctx context.Context) {
	close(a.started)
	<-ctx.Done()
	close(a.stopped)
}

type startupDiffWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (w *startupDiffWriter) Write(data []byte) (int, error) {
	if strings.Contains(ansi.Strip(string(data)), "1 file changed") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(data), nil
}

func TestStartupLoadsDiffsWhileModelDiscoveryIsBlocked(t *testing.T) {
	testenv.WingmanHome(t)
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("startup diff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := &blockedModelAgent{
		uiTestAgent: newUITestAgent(nil),
		started:     make(chan struct{}),
		stopped:     make(chan struct{}),
	}
	agent.workspace = &corecode.Workspace{RootPath: dir}
	ctx, cancel := context.WithCancel(context.Background())
	app := New(ctx, agent, "session")
	output := &startupDiffWriter{ready: make(chan struct{})}
	app.WithTerminal(inline.NewTerminal(inline.WithIO(strings.NewReader(""), output, func() (int, int) {
		return 180, 24
	})))
	runDone := make(chan error, 1)
	started := time.Now()
	go func() { runDone <- app.Run() }()
	t.Cleanup(func() {
		cancel()
		app.stop()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("TUI run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TUI did not stop")
		}
	})

	select {
	case <-agent.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model discovery did not start")
	}
	select {
	case <-output.ready:
		t.Logf("diff panel rendered after %s while model discovery was still blocked", time.Since(started))
	case <-time.After(5 * time.Second):
		t.Fatal("model discovery blocked the first diff panel")
	}

	app.stop()
	select {
	case <-agent.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("quitting the TUI did not cancel model discovery")
	}
	if ctx.Err() != nil {
		t.Fatal("TUI quit cancelled its caller's context")
	}
}

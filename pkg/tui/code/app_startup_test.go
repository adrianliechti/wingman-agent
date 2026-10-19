package code

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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
	release <-chan struct{}
}

func (a *blockedModelAgent) FetchModels(ctx context.Context) error {
	close(a.started)
	<-ctx.Done()
	close(a.stopped)
	if a.release != nil {
		<-a.release
	}
	return ctx.Err()
}

func TestRunWaitsForStartupBeforeReturning(t *testing.T) {
	testenv.WingmanHome(t)
	dir := t.TempDir()
	for _, stop := range []string{"quit", "context cancellation"} {
		t.Run(stop, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				agent := &blockedModelAgent{
					uiTestAgent: newUITestAgent(nil),
					started:     make(chan struct{}),
					stopped:     make(chan struct{}),
					release:     release,
				}
				agent.workspace = &corecode.Workspace{RootPath: dir}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				app := New(ctx, agent, "session")
				app.WithTerminal(inline.NewTerminal(inline.WithIO(strings.NewReader(""), io.Discard, func() (int, int) {
					return 100, 24
				})))
				runDone := make(chan error, 1)
				go func() { runDone <- app.Run() }()
				<-agent.started
				if stop == "quit" {
					app.stop()
				} else {
					cancel()
				}
				<-agent.stopped
				synctest.Wait()
				select {
				case err := <-runDone:
					t.Fatalf("Run returned before startup finished: %v", err)
				default:
				}
				release <- struct{}{}
				if err := <-runDone; err != nil {
					t.Fatal(err)
				}
				select {
				case <-app.quit:
				default:
					t.Fatal("Run did not stop its event pumps")
				}
			})
		})
	}
}

type startupDiffWriter struct {
	ready     chan struct{}
	typed     chan struct{}
	once      sync.Once
	typedOnce sync.Once
}

func (w *startupDiffWriter) Write(data []byte) (int, error) {
	text := ansi.Strip(string(data))
	if strings.Contains(text, "1 file changed") {
		w.once.Do(func() { close(w.ready) })
	}
	if strings.Contains(text, "typing with changes") {
		w.typedOnce.Do(func() { close(w.typed) })
	}
	return len(data), nil
}

func TestStartupLoadsDiffsAndAcceptsInputWhileModelDiscoveryIsBlocked(t *testing.T) {
	testenv.WingmanHome(t)
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.js"), []byte(strings.Repeat("const value = 123;", 200_000)), 0o600); err != nil {
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
	inputR, inputW := io.Pipe()
	output := &startupDiffWriter{ready: make(chan struct{}), typed: make(chan struct{})}
	app.WithTerminal(inline.NewTerminal(inline.WithIO(inputR, output, func() (int, int) {
		return 180, 24
	})))
	runDone := make(chan error, 1)
	started := time.Now()
	go func() { runDone <- app.Run() }()
	t.Cleanup(func() {
		cancel()
		app.stop()
		inputW.Close()
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
	if _, err := io.WriteString(inputW, "typing with changes"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.typed:
	case <-time.After(5 * time.Second):
		t.Fatal("chat typing froze with the startup diff visible")
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

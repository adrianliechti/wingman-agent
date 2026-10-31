//go:build !windows

package terminal

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func reviewWorkspaceTerminalGated(t *testing.T, m *Manager, script string) (*Session, func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "go")
	session, err := m.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", `while [ ! -e "$GATE" ]; do sleep 0.01; done; ` + script}, Env: map[string]*string{"GATE": &gate}}, DefaultCols, DefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	return session, func() {
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewWorkspaceTerminalBehindSubscriberIsCutOffAndRecovers(t *testing.T) {
	m := NewManager(t.TempDir())
	defer m.Close()
	session, open := reviewWorkspaceTerminalGated(t, m, `head -c 600000 /dev/zero | tr '\0' a; printf '\nbehind-marker\n'; sleep 30`)
	_, out, cancel := session.Subscribe()
	defer cancel()
	open()
	recovered := false
	deadline := time.Now().Add(10 * time.Second)
	for !recovered && time.Now().Before(deadline) {
		snapshot, _, cancel := session.Subscribe()
		cancel()
		recovered = bytes.Contains(snapshot, []byte("behind-marker"))
		time.Sleep(10 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("output never reached the scrollback")
	}
	received := 0
	for chunk := range out {
		received += len(chunk)
	}
	if session.Exited() || received >= 600000 {
		t.Fatalf("a subscriber a whole scrollback behind must be cut off while the session runs: %d bytes", received)
	}
	snapshot, live, cancel := session.Subscribe()
	defer cancel()
	if len(snapshot) > maxScrollback+compactSlack || !bytes.HasSuffix(bytes.TrimRight(snapshot, "\r\n"), []byte("behind-marker")) || !utf8.Valid(snapshot) {
		t.Fatalf("recovery snapshot: %d bytes", len(snapshot))
	}
	_ = session.Close()
	for range live {
	}
}

func TestReviewWorkspaceTerminalLateSubscriberAfterExit(t *testing.T) {
	m := NewManager(t.TempDir())
	defer m.Close()
	session, open := reviewWorkspaceTerminalGated(t, m, `printf 'last-words\n'; exit 9`)
	open()
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("no exit")
	}
	if code, known := session.ExitCode(); !known || code != 9 {
		t.Fatalf("exit %d known=%v", code, known)
	}
	snapshot, out, cancel := session.Subscribe()
	defer cancel()
	if !bytes.Contains(snapshot, []byte("last-words")) {
		t.Fatalf("snapshot %q", snapshot)
	}
	if _, ok := <-out; ok {
		t.Fatal("exited session delivered live output")
	}
	if err := session.Write([]byte("x")); err == nil {
		t.Fatal("write to an exited session succeeded")
	}
	if err := session.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if session.HasForegroundProcess() {
		t.Fatal("exited session reports a foreground process")
	}
	killed, err := m.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}, DefaultCols, DefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Remove(killed.ID()) || m.Remove(killed.ID()) {
		t.Fatal("remove must succeed exactly once")
	}
	if _, known := killed.ExitCode(); known {
		t.Fatal("a killed session must not report an exit code")
	}
}

func TestReviewWorkspaceTerminalConcurrentSubscribeCancelClose(t *testing.T) {
	m := NewManager(t.TempDir())
	defer m.Close()
	session, err := m.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "yes 0123456789abcdef"}}, DefaultCols, DefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, out, cancel := session.Subscribe()
				for range i % 3 {
					<-out
				}
				cancel()
				if i%2 == 0 {
					for range out {
					}
				}
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		var closers sync.WaitGroup
		closers.Go(func() { _ = session.Close() })
		closers.Go(func() { m.Remove(session.ID()) })
		closers.Go(func() { _ = session.Close() })
		closers.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent close hung")
	}
	close(stop)
	wg.Wait()
	_, out, cancel := session.Subscribe()
	cancel()
	if _, ok := <-out; ok {
		t.Fatal("closed session streams output")
	}
	if m.Get(session.ID()) != nil {
		t.Fatal("removed session still listed")
	}
}

// Bug: Resize and HasForegroundProcess call os.File.Fd on the PTY master without synchronizing with Close, a data race (-race) that can also ioctl a recycled descriptor; they must be serialized with closing the PTY.
func TestReviewWorkspaceTerminalResizeAndStatusRaceWithClose(t *testing.T) {
	m := NewManager(t.TempDir())
	defer m.Close()
	for range 10 {
		session, err := m.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}, DefaultCols, DefaultRows)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				for n := 0; !session.Exited(); n++ {
					if i == 0 {
						_ = session.Resize(80+n%2, 24)
					} else {
						session.HasForegroundProcess()
					}
				}
			})
		}
		time.Sleep(5 * time.Millisecond)
		m.Remove(session.ID())
		wg.Wait()
	}
}

func TestReviewWorkspaceTerminalCatalogueEvents(t *testing.T) {
	m := NewManager(t.TempDir())
	initial, events, cancel := m.Subscribe()
	defer cancel()
	if len(initial) != 0 {
		t.Fatal("new manager lists sessions")
	}
	session, err := m.CreateCommand(CommandSpec{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}, DefaultCols, DefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	next := func() Event {
		t.Helper()
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("events closed")
			}
			return event
		case <-time.After(10 * time.Second):
			t.Fatalf("missing event after %v", kinds)
		}
		return Event{}
	}
	for len(kinds) < 2 {
		event := next()
		if event.Session != session {
			t.Fatal("event for another session")
		}
		kinds = append(kinds, event.Kind)
	}
	if strings.Join(kinds, ",") != "created,exited" {
		t.Fatalf("events %v", kinds)
	}
	if !m.Remove(session.ID()) {
		t.Fatal("exited session not retained")
	}
	if event := next(); event.Kind != "removed" || event.Session != session {
		t.Fatalf("remove event %+v", event)
	}
	m.Close()
	if _, ok := <-events; ok {
		t.Fatal("observer outlived the manager")
	}
	cancel()
	if list, after, _ := m.Subscribe(); len(list) != 0 {
		t.Fatal("closed manager lists sessions")
	} else if _, ok := <-after; ok {
		t.Fatal("subscription to a closed manager is open")
	}
	if _, err := m.CreateCommand(CommandSpec{Path: "/bin/sh"}, DefaultCols, DefaultRows); err == nil {
		t.Fatal("closed manager created a session")
	}
}

func TestReviewWorkspaceTerminalSafeTailProperties(t *testing.T) {
	alphabet := []string{"a", "é", "🌍", "中", "\n", "\r\n", "\x1b[31m", "\x1b]0;t\x07", " "}
	rng := rand.New(rand.NewPCG(3, 4))
	for round := range 3000 {
		var b strings.Builder
		for range rng.IntN(3000) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		data := []byte(b.String())
		limit := rng.IntN(len(data) + 8)
		got := SafeTail(data, limit)
		if len(got) > limit || !bytes.HasSuffix(data, got) || !utf8.Valid(got) || len(data) > limit && len(got) < limit-(4<<10) {
			t.Fatalf("round %d: SafeTail(%d bytes, %d) = %d bytes %q", round, len(data), limit, len(got), got[:min(len(got), 16)])
		}
	}
}

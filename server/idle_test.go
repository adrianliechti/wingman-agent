package server

import (
	"context"
	"testing"
	"time"
)

// A host is idle while no client follows it and nothing works.
func TestServerIsIdleWithoutClients(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	if !s.Idle() {
		t.Fatal("a new host is not idle")
	}
	client := protocolTestClient(t, endpoint, "idle-test")
	if s.Idle() {
		t.Fatal("a host is idle while a client follows it")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = client.Shutdown(ctx)
	for !s.Idle() {
		if ctx.Err() != nil {
			t.Fatal("the host stays busy after its client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

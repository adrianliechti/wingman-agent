package agent

import (
	"context"
	"testing"
)

func TestConfirmWithoutUIFailsClosed(t *testing.T) {
	a := &Agent{}
	allowed, err := a.confirm(context.Background(), "run dangerous command?")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("missing UI approved a confirmation")
	}
}

func TestEffortReturnsIndependentValues(t *testing.T) {
	a := &Agent{}
	_, values := a.Effort("")
	values[0] = "changed"
	_, again := a.Effort("")
	if again[0] != "auto" {
		t.Fatalf("caller mutated shared effort values: %v", again)
	}
}

func TestClosedAgentRejectsNewSession(t *testing.T) {
	a := &Agent{closed: true}
	if _, err := a.NewSession(context.Background()); err == nil {
		t.Fatal("closed agent created a session")
	}
}

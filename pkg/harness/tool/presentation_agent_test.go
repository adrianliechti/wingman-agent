package tool

import "testing"

func TestAgentControlsKeepHintsInRestoredHistory(t *testing.T) {
	for _, tc := range []struct{ name, title, args string }{
		{"agent_output", "Read agent output", `{"id":"child-1"}`},
		{"agent_stop", "Stop agent", `{"id":"child-1"}`},
		{"agent_send", "Follow up with agent", `{"id":"child-1","message":"check errors"}`},
	} {
		got, want := Present(tc.title, "", tc.args, false), Present(tc.name, "", tc.args, false)
		if got != want {
			t.Errorf("%s restored as %#v, want %#v", tc.name, got, want)
		}
	}
}

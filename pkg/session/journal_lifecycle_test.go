package session

import (
	"bytes"
	"os"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func TestDeletedJournalRejectsLateWrites(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, "deleted")
	if err != nil {
		t.Fatal(err)
	}
	message := userMsg("original")
	if err := j.AppendEvents([]harness.RuntimeEvent{{ID: "first", Sequence: 1, Type: harness.EventMessage, Message: &message}}); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "deleted"); err != nil {
		t.Fatal(err)
	}
	late := []harness.RuntimeEvent{{ID: "late", Sequence: 2, Type: harness.EventMessage, Message: &message}}
	if err := j.AppendEvents(late); err == nil {
		t.Error("deleted journal accepted a late write")
	}
	if _, err := os.Stat(sessionPath(dir, "deleted")); !os.IsNotExist(err) {
		t.Fatalf("deleted journal was recreated: %v", err)
	}
	// Reusing the ID is allowed, but writers from its previous lifetime must
	// never append into the replacement journal.
	if err := Save(dir, "deleted", harness.State{Messages: []harness.Message{userMsg("replacement")}}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendEvents(late); err == nil {
		t.Error("stale writer appended into a replacement journal")
	}
	loaded, err := Load(dir, "deleted")
	if err != nil || len(loaded.State.Messages) != 1 || loaded.State.Messages[0].Content[0].Text != "replacement" {
		t.Fatalf("replacement journal changed: %+v, %v", loaded, err)
	}
}

func TestJournalPreservesCompleteRecordWithoutFinalNewline(t *testing.T) {
	dir, id := t.TempDir(), "no-final-newline"
	if err := Save(dir, id, harness.State{Messages: []harness.Message{userMsg("first"), userMsg("second")}}); err != nil {
		t.Fatal(err)
	}
	path := sessionPath(dir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(data, []byte{'\n'}), 0644); err != nil {
		t.Fatal(err)
	}
	// Simulate reopening after a process restart.
	journalCoordinators.Delete(path)
	loaded, err := Load(dir, id)
	if err != nil || len(loaded.State.Messages) != 2 {
		t.Fatalf("load before append = %+v, %v", loaded, err)
	}
	j, err := OpenJournal(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	message := userMsg("third")
	sequence := loaded.State.Events[len(loaded.State.Events)-1].Sequence + 1
	if err := j.AppendEvents([]harness.RuntimeEvent{{ID: "third", Sequence: sequence, Type: harness.EventMessage, Message: &message}}); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(dir, id)
	if err != nil || len(loaded.State.Messages) != 3 || loaded.State.Messages[1].Content[0].Text != "second" {
		t.Fatalf("complete final record was lost: %+v, %v", loaded, err)
	}
}

package task

import (
	"path/filepath"
	"testing"
)

func BenchmarkTaskActivity(b *testing.B) {
	registry, err := NewFileRegistry(filepath.Join(b.TempDir(), "tasks.json"))
	if err != nil {
		b.Fatal(err)
	}
	defer registry.Close()
	task := &Task{ID: "task", registry: registry}
	registry.tasks = []*Task{task}
	for b.Loop() {
		task.SetActivity("Reading main.go")
	}
}

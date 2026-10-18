package fs_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

func TestEditToolTolerance(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()

	editTool := EditTool(root)

	edit := func(t *testing.T, name, content string, args map[string]any) (string, error) {
		t.Helper()
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": name}); err != nil {
			t.Fatal(err)
		}
		args["file_path"] = name
		_, err := editTool.Execute(context.Background(), args)
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(got), err
	}

	tests := []struct {
		name    string
		content string
		args    map[string]any
		want    string // expected file content on success
		wantErr string // expected error substring on failure; file must be unchanged
	}{
		{
			name:    "trailing whitespace on the last matched line is replaced",
			content: "hello   \nworld   \nnext",
			args:    map[string]any{"old_string": "hello\nworld", "new_string": "goodbye\nearth"},
			want:    "goodbye\nearth\nnext",
		},
		{
			name:    "mid-line match keeps the rest of the line",
			content: "hello world   \nnext",
			args:    map[string]any{"old_string": "hello\t", "new_string": "bye"},
			want:    "bye world   \nnext",
		},
		{
			name:    "mixed curly quote variants count as multiple occurrences",
			content: "say “Hello” and ”Hello”",
			args:    map[string]any{"old_string": `"Hello"`, "new_string": `"World"`},
			wantErr: "2 occurrences",
		},
		{
			name:    "replace_all covers every curly quote variant",
			content: "say “Hello” and ”Hello”",
			args:    map[string]any{"old_string": `"Hello"`, "new_string": `"World"`, "replace_all": true},
			want:    "say “World” and “World”",
		},
		{
			name:    "replace_all allows expanding replacement in curly-quoted content",
			content: "say “hello” and say “hello”",
			args:    map[string]any{"old_string": `say "hello"`, "new_string": `say "hello" again`, "replace_all": true},
			want:    "say “hello” again and say “hello” again",
		},
		{
			name:    "gutter-only blank line does not strip to empty search string",
			content: "abc",
			args:    map[string]any{"old_string": "1\t", "new_string": "X", "replace_all": true},
			wantErr: "could not find old_string",
		},
		{
			name:    "indentation match with curly quotes in comment preserves straight code quotes",
			content: "\t# “note”\n\tprint(\"hi\")\n",
			args: map[string]any{
				"old_string": "# “note”\nprint(\"hi\")",
				"new_string": "# “note”\nprint(\"bye\")",
			},
			want: "\t# “note”\n\tprint(\"bye\")\n",
		},
		{
			name:    "read line-number gutter in old_string and new_string is stripped",
			content: "a := 1\nb := 2\n",
			args:    map[string]any{"old_string": "1\ta := 1\n2\tb := 2", "new_string": "1\ta := 1\n2\tb := 3"},
			want:    "a := 1\nb := 3\n",
		},
		{
			name:    "read line-number gutter only in old_string",
			content: "a := 1\nb := 2\n",
			args:    map[string]any{"old_string": "1\ta := 1\n2\tb := 2", "new_string": "a := 1\nb := 3"},
			want:    "a := 1\nb := 3\n",
		},
		{
			name:    "gutter is not stripped unless every line has one",
			content: "foo\nbar\n",
			args:    map[string]any{"old_string": "1\tfoo\nbar", "new_string": "x"},
			wantErr: "could not find old_string",
		},
		{
			name:    "old_string over-indented by one tab is re-indented",
			content: "func f() {\n\t\tif a {\n\t\t\tb()\n\t\t}\n}\n",
			args: map[string]any{
				"old_string": "\t\t\tif a {\n\t\t\t\tb()\n\t\t\t}",
				"new_string": "\t\t\tif a {\n\t\t\t\tc()\n\t\t\t\td()\n\t\t\t}",
			},
			want: "func f() {\n\t\tif a {\n\t\t\tc()\n\t\t\td()\n\t\t}\n}\n",
		},
		{
			name:    "old_string under-indented is re-indented",
			content: "\t\tif a {\n\t\t\tb()\n\t\t}\n",
			args: map[string]any{
				"old_string": "if a {\n\tb()\n}",
				"new_string": "if a {\n\tc()\n}",
			},
			want: "\t\tif a {\n\t\t\tc()\n\t\t}\n",
		},
		{
			name:    "old_string with trailing newline keeps the newline",
			content: "\tx := 1\n\ty := 2\n\tz := 3\n",
			args:    map[string]any{"old_string": "\t\tx := 1\n\t\ty := 2\n", "new_string": "\t\tx := 9\n"},
			want:    "\tx := 9\n\tz := 3\n",
		},
		{
			name:    "inconsistent indentation shift is rejected",
			content: "\tif a {\n\t\tb()\n\t}\n",
			args: map[string]any{
				"old_string": "\t\tif a {\n\t\tb()\n\t}",
				"new_string": "x",
			},
			wantErr: "could not find old_string",
		},
		{
			name:    "ambiguous indentation-shifted block is rejected",
			content: "\tif a {\n\t\tb()\n\t}\n\tif a {\n\t\tb()\n\t}\n",
			args: map[string]any{
				"old_string": "\t\tif a {\n\t\t\tb()\n\t\t}",
				"new_string": "\t\tif a {\n\t\t\tc()\n\t\t}",
			},
			wantErr: "2 occurrences",
		},
		{
			name:    "replace_all handles every indentation-shifted block",
			content: "\tif a {\n\t\tb()\n\t}\n\tif a {\n\t\tb()\n\t}\n",
			args: map[string]any{
				"old_string":  "\t\tif a {\n\t\t\tb()\n\t\t}",
				"new_string":  "\t\tif a {\n\t\t\tc()\n\t\t}",
				"replace_all": true,
			},
			want: "\tif a {\n\t\tc()\n\t}\n\tif a {\n\t\tc()\n\t}\n",
		},
		{
			name:    "different text is never matched by indentation tolerance",
			content: "\tfoo()\n",
			args:    map[string]any{"old_string": "\t\tbar()", "new_string": "x"},
			wantErr: "could not find old_string",
		},
		{
			name:    "extra blank line in unchanged context still matches and preserves file spacing",
			content: "\t\tif ok {\n\t\t\ta()\n\t\t}\n\t}\nfunc Next() {\n\trun(\"old\")\n}\n",
			args: map[string]any{
				"old_string": "\t\t\ta()\n\t\t}\n\t}\n\n\tfunc Next() {\n\t\trun(\"old\")",
				"new_string": "\t\t\ta()\n\t\t}\n\t}\n\n\tfunc Next() {\n\t\trun(\"new\")",
			},
			want: "\t\tif ok {\n\t\t\ta()\n\t\t}\n\t}\nfunc Next() {\n\trun(\"new\")\n}\n",
		},
		{
			name:    "missing blank line in old_string still matches when non-empty lines and indent shift match",
			content: "func Alpha() {\n\tx := 1\n\n\ty := 2\n}\n",
			args: map[string]any{
				"old_string": "func Alpha() {\n\tx := 1\n\ty := 2\n}",
				"new_string": "func Alpha() {\n\tx := 10\n\ty := 20\n}",
			},
			want: "func Alpha() {\n\tx := 10\n\ty := 20\n}\n",
		},
		{
			name:    "blank-line tolerance deletes the whole changed line",
			content: "alpha\n\nbeta\ngamma\n",
			args:    map[string]any{"old_string": "alpha\nbeta\ngamma", "new_string": "alpha\ngamma"},
			want:    "alpha\n\ngamma\n",
		},
		{
			name:    "blank-line tolerance removes the final newline when requested",
			content: "alpha\n\nbeta\n",
			args:    map[string]any{"old_string": "alpha\nbeta\n", "new_string": "alpha\nchanged"},
			want:    "alpha\n\nchanged",
		},
		{
			name:    "blank-line tolerance adds the final newline when requested",
			content: "alpha\n\nbeta",
			args:    map[string]any{"old_string": "alpha\nbeta", "new_string": "alpha\nchanged\n"},
			want:    "alpha\n\nchanged\n",
		},
		{
			name:    "blank-line tolerance honors newline-only changes",
			content: "alpha\n\nbeta\n",
			args:    map[string]any{"old_string": "alpha\nbeta\n", "new_string": "alpha\nbeta"},
			want:    "alpha\n\nbeta",
		},
		{
			name:    "anchor indentation tolerance still requires the old final newline",
			content: "alpha\n\tbeta",
			args:    map[string]any{"old_string": "\talpha\n\tbeta\n", "new_string": "\talpha\n\tchanged\n"},
			wantErr: "could not find old_string",
		},
		{
			name:    "blank-line tolerance consumes boundary blank lines in full replacements",
			content: "before\n\nalpha\n\nbeta\n\nafter\n",
			args:    map[string]any{"old_string": "\nalpha\nbeta\n\n", "new_string": "\nchanged\n"},
			want:    "before\n\nchanged\nafter\n",
		},
		{
			name:    "blank-line tolerance rejects missing boundary context",
			content: "before\nalpha\n\nbeta\n\nafter\n",
			args:    map[string]any{"old_string": "\nalpha\nbeta\n\n", "new_string": "changed\n"},
			wantErr: "could not find old_string",
		},
		{
			name:    "ambiguous blank-line matches are rejected",
			content: "alpha\n\nbeta\nalpha\n\nbeta\n",
			args:    map[string]any{"old_string": "alpha\nbeta", "new_string": "alpha\nchanged"},
			wantErr: "2 occurrences",
		},
		{
			name:    "replace_all covers blank-line matches",
			content: "alpha\n\nbeta\nalpha\n\nbeta\n",
			args:    map[string]any{"old_string": "alpha\nbeta", "new_string": "alpha\nchanged", "replace_all": true},
			want:    "alpha\n\nchanged\nalpha\n\nchanged\n",
		},
		{
			name:    "replace_all skips overlapping boundary blank lines",
			content: "\nalpha\n\nbeta\n\nalpha\n\nbeta\n\n",
			args:    map[string]any{"old_string": "\nalpha\nbeta\n\n", "new_string": "changed\n", "replace_all": true},
			want:    "changed\nalpha\n\nbeta\n\n",
		},
		{
			name:    "blank-line tolerance normalizes CRLF edit arguments",
			content: "alpha\n\nbeta\n",
			args:    map[string]any{"old_string": "alpha\r\nbeta\r\n", "new_string": "alpha\r\nchanged\r\n"},
			want:    "alpha\n\nchanged\n",
		},
	}

	for i, tt := range tests {
		for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
			t.Run(tt.name+"/"+ending.name, func(t *testing.T) {
				name := fmt.Sprintf("case%d.txt", i)
				content := strings.ReplaceAll(tt.content, "\n", ending.value)
				got, err := edit(t, name, content, tt.args)

				if tt.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
					}
					if got != content {
						t.Fatalf("file changed despite error: %q", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if want := strings.ReplaceAll(tt.want, "\n", ending.value); got != want {
					t.Fatalf("content = %q, want %q", got, want)
				}
			})
		}
	}

	t.Run("CRLF files keep their line endings through tolerant matching", func(t *testing.T) {
		content := "func f() {\r\n\t\tif a {\r\n\t\t\tb()\r\n\t\t}\r\n}\r\n"
		got, err := edit(t, "crlf_indent.txt", content, map[string]any{
			"old_string": "\t\t\tif a {\n\t\t\t\tb()\n\t\t\t}",
			"new_string": "\t\t\tif a {\n\t\t\t\tc()\n\t\t\t}",
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := "func f() {\r\n\t\tif a {\r\n\t\t\tc()\r\n\t\t}\r\n}\r\n"; got != want {
			t.Fatalf("content = %q, want %q", got, want)
		}

		got, err = edit(t, "crlf_gutter.txt", "a := 1\r\nb := 2\r\n", map[string]any{
			"old_string": "1\ta := 1\n2\tb := 2",
			"new_string": "a := 1\nb := 3",
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := "a := 1\r\nb := 3\r\n"; got != want {
			t.Fatalf("content = %q, want %q", got, want)
		}
	})

	t.Run("mismatch error shows the closest region instead of the file head", func(t *testing.T) {
		content := "package x\n\nfunc Alpha() {\n\treturn 1\n}\nfunc Beta() {\n\treturn 2\n}\n"
		_, err := edit(t, "closest.go", content, map[string]any{
			"old_string": "func Beta() {\n\treturn 3\n}",
			"new_string": "x",
		})
		if err == nil {
			t.Fatal("expected mismatch error")
		}
		for _, want := range []string{"could not find old_string", "Closest match (lines 3-8", "6\tfunc Beta() {", "7\t\treturn 2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "File starts with") {
			t.Errorf("unexpected file-head fallback: %v", err)
		}
	})

	t.Run("mismatch error near the top of the file stays in range", func(t *testing.T) {
		_, err := edit(t, "top.go", "alpha beta\ngamma delta\n", map[string]any{
			"old_string": "zzzz\nalpha beta\nqqqq",
			"new_string": "x",
		})
		if err == nil || !strings.Contains(err.Error(), "Closest match (lines 1-") || !strings.Contains(err.Error(), "1\talpha beta") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("mismatch error falls back to the file head without any anchor", func(t *testing.T) {
		_, err := edit(t, "noanchor.txt", "one\ntwo\n", map[string]any{
			"old_string": "completely different",
			"new_string": "x",
		})
		if err == nil || !strings.Contains(err.Error(), "File starts with") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("mismatch error locates closest region using identifier tokens when line structure differs", func(t *testing.T) {
		content := "package truncation\n\nfunc budgetFor(name string) int {\n\tswitch name {\n\tcase \"grep\":\n\t\treturn 1\n\tcase \"shell\", \"exec_command\", \"exec_session\":\n\t\treturn 2\n\tdefault:\n\t\treturn 3\n\t}\n}\n"
		_, err := edit(t, "truncation.go", content, map[string]any{
			"old_string": "var defaultTools = map[string]bool{\n\t\"exec_command\": true,\n\t\"exec_session\": true,\n\t\"shell\": true,\n}",
			"new_string": "x",
		})
		if err == nil {
			t.Fatal("expected mismatch error")
		}
		for _, want := range []string{"Closest match", `"shell", "exec_command", "exec_session"`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err, want)
			}
		}
	})
}

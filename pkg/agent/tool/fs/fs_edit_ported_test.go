package fs

import (
	"strings"
	"testing"
)

// Ported from pi's edit tool fuzzy-matching and CRLF tests. These exercise the
// replacement core directly: the tool normalizes line endings before calling it
// and restores them afterwards.
func TestApplyEditOpFuzzyAndLineEndingCases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		old     string
		new     string
		want    string
		wantErr string
	}{
		{
			name:    "trailing whitespace is ignored when matching",
			content: "line one   \nline two\nline three\n",
			old:     "line one\nline two",
			new:     "replaced",
			want:    "replaced\nline three\n",
		},
		{
			name:    "exact match wins over a fuzzy candidate",
			content: "say “hello”\nsay \"hello\"\n",
			old:     "say \"hello\"",
			new:     "say \"bye\"",
			want:    "say “hello”\nsay \"bye\"\n",
		},
		{
			name:    "duplicates are detected after normalization",
			content: "hello world   \nhello world\n",
			old:     "hello world",
			new:     "bye",
			wantErr: "found 2 occurrences",
		},
		{
			name:    "fuzzy matching does not invent matches",
			content: "hello world\n",
			old:     "goodbye world",
			new:     "x",
			wantErr: "could not find old_string",
		},
		{
			name:    "LF old_string matches CRLF content once normalized",
			content: normalizeToLF("first\r\nsecond\r\nthird\r\n"),
			old:     "second",
			new:     "REPLACED",
			want:    "first\nREPLACED\nthird\n",
		},
		{
			name:    "duplicates across CRLF and LF spellings",
			content: normalizeToLF("hello\r\nworld\r\n---\r\nhello\nworld\n"),
			old:     "hello\nworld",
			new:     "x",
			wantErr: "found 2 occurrences",
		},
		{
			name:    "replace_all rewrites every fuzzy occurrence",
			content: "foo—bar\nfoo–bar\n",
			old:     "foo-bar",
			new:     "baz",
			want:    "baz\nbaz\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := editOp{oldText: tc.old, newText: tc.new, replaceAll: strings.HasPrefix(tc.name, "replace_all")}
			got, err := applyEditOp(tc.content, op, "f.txt")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if got := restoreLineEndings(normalizeToLF("a\r\nb\r\n"), detectLineEnding("a\r\nb\r\n")); got != "a\r\nb\r\n" {
		t.Fatalf("CRLF round trip = %q", got)
	}
}

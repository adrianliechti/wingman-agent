// Adapted from JetBrains codex-acp, Copyright 2025 JetBrains s.r.o.
// SPDX-License-Identifier: Apache-2.0
// Go implementation of src/CodexFileCitations.ts at 3f6d371.

package codex

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Match Codex's assistant_directives parser, including literal backslashes in
// paths. This follows codex-acp's FileCitationStream: directives split across
// deltas wait for completion, while code and escaped directives stay literal.
type citationStream struct {
	cwd, pending string
	previous     byte
	codeRun      int
}

func (s *citationStream) push(delta string) string {
	text := s.pending + delta
	var out strings.Builder
	i := 0
	for i < len(text) {
		if text[i] == '`' {
			end := i
			for end < len(text) && text[end] == '`' {
				end++
			}
			if end == len(text) {
				break
			}
			run := end - i
			if s.codeRun == 0 {
				s.codeRun = run
			} else if s.codeRun == run {
				s.codeRun = 0
			}
			out.WriteString(text[i:end])
			i = end
			continue
		}
		before := s.previous
		if i > 0 {
			before = text[i-1]
		}
		if text[i] == ':' && s.codeRun == 0 && before != ':' && before != '\\' {
			path, end, partial := parseCitation(text, i, false)
			if end == 0 && !partial {
				path, end, partial = parseCitation(text, i, true)
			}
			if partial {
				break
			}
			if end > 0 {
				out.WriteString(citationLink(path, s.cwd))
				i = end
				continue
			}
		}
		out.WriteByte(text[i])
		i++
	}
	if i > 0 {
		s.previous = text[i-1]
	}
	s.pending = text[i:]
	return out.String()
}

func (s *citationStream) flush() string {
	text := s.pending
	s.pending = ""
	return text
}

func renderCitations(text, cwd string) string {
	s := citationStream{cwd: cwd}
	return s.push(text) + s.flush()
}

func parseCitation(text string, start int, escapedQuotes bool) (path string, end int, partial bool) {
	limit := min(len(text), start+4096)
	ended := func() (string, int, bool) { return "", 0, limit == len(text) }
	i := start
	for i < limit && text[i] == ':' {
		i++
	}
	if i == limit {
		return ended()
	}
	if i-start > 3 {
		return "", 0, false
	}
	for _, expected := range []byte("codex-file-citation{") {
		if i == limit {
			return ended()
		}
		if text[i] != expected {
			return "", 0, false
		}
		i++
	}
	spaces := func() {
		for i < limit && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
	}
	keys := map[string]bool{}
	for {
		spaces()
		if i == limit {
			return ended()
		}
		if text[i] == '}' {
			if path != "" {
				return path, i + 1, false
			}
			return "", 0, false
		}
		keyStart := i
		for i < limit && citationNameByte(text[i]) {
			i++
		}
		if i == limit {
			return ended()
		}
		key := text[keyStart:i]
		if key == "" || keys[key] {
			return "", 0, false
		}
		keys[key] = true
		spaces()
		if i == limit {
			return ended()
		}
		if text[i] != '=' {
			return "", 0, false
		}
		i++
		spaces()
		if i == limit {
			return ended()
		}
		var value strings.Builder
		quote := text[i]
		if quote == '"' || quote == '\'' {
			i++
			for {
				if i == limit {
					return ended()
				}
				if text[i] == quote {
					break
				}
				if text[i] == '\n' || text[i] == '\r' {
					return "", 0, false
				}
				if escapedQuotes && text[i] == '\\' && i+1 < limit && text[i+1] == quote {
					i++
				}
				value.WriteByte(text[i])
				i++
			}
			i++
		} else {
			for i < limit && !strings.ContainsRune(" \t}\n\r", rune(text[i])) {
				value.WriteByte(text[i])
				i++
			}
			if i == limit {
				return ended()
			}
			if value.Len() == 0 {
				return "", 0, false
			}
		}
		if key == "path" {
			path = value.String()
		}
	}
}

func citationNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

var absoluteCitation = regexp.MustCompile(`^(?:[A-Za-z]:[\\/]|[A-Za-z][\w+.-]*://)`)

func citationLink(path, cwd string) string {
	target := path
	if !filepath.IsAbs(path) && !absoluteCitation.MatchString(path) {
		target = filepath.Join(cwd, path)
	}
	if strings.ContainsAny(target, " \t\n\r()<>") {
		target = "<" + escapeCitation(target, "<>") + ">"
	} else {
		target = escapeCitation(target, "")
	}
	return "[" + escapeCitation(path, "[]") + "](" + target + ")"
}

func escapeCitation(text, special string) string {
	const punctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
	var out strings.Builder
	for i := range len(text) {
		if strings.ContainsRune(special, rune(text[i])) || text[i] == '\\' && i+1 < len(text) && strings.ContainsRune(punctuation, rune(text[i+1])) {
			out.WriteByte('\\')
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

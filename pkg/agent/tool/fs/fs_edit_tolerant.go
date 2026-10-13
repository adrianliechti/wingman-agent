package fs

import (
	"fmt"
	"regexp"
	"strings"
)

// readGutter matches the "<line>\t" prefix that the read tool puts on every line.
var readGutter = regexp.MustCompile(`^ *[0-9]+\t`)

// stripReadLineNumbers removes a copied read-output gutter. It only applies when
// every non-empty line of oldText carries one, so ordinary text is never altered.
func stripReadLineNumbers(oldText, newText string) (string, string, bool) {
	strippedOld, ok := stripGutter(oldText)
	// A gutter-only old_string (e.g. a numbered blank line) strips to nothing;
	// an empty search would match everywhere and bypass the empty-string guard.
	if !ok || strings.TrimSpace(strippedOld) == "" {
		return "", "", false
	}
	if strippedNew, ok := stripGutter(newText); ok {
		newText = strippedNew
	}
	return strippedOld, newText, true
}

func stripGutter(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	seen := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !readGutter.MatchString(line) {
			return text, false
		}
		seen = true
	}
	if !seen {
		return text, false
	}
	for i, line := range lines {
		lines[i] = readGutter.ReplaceAllString(line, "")
	}
	return strings.Join(lines, "\n"), true
}

type contentLine struct {
	start, end int // byte span excluding the newline
}

func splitContentLines(content string) []contentLine {
	var lines []contentLine
	start := 0
	for {
		i := strings.IndexByte(content[start:], '\n')
		if i < 0 {
			lines = append(lines, contentLine{start, len(content)})
			return lines
		}
		lines = append(lines, contentLine{start, start + i})
		start += i + 1
	}
}

func leadingIndent(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func lineKey(line string) string {
	return normalizeForFuzzyMatch(strings.TrimLeft(line, " \t"))
}

// indentShift describes how oldText's indentation differs from the file's: the
// file has extra (add) or lacks (remove) a whitespace prefix on every line.
type indentShift struct {
	add, remove string
}

// matchIndentShift reports whether window matches oldLines line for line,
// ignoring leading whitespace, with one uniform indentation shift.
func matchIndentShift(content string, window []contentLine, oldLines []string) (indentShift, bool) {
	var shift indentShift
	decided := false
	for j, cl := range window {
		fileLine := content[cl.start:cl.end]
		oldLine := oldLines[j]
		if lineKey(fileLine) != lineKey(oldLine) {
			return shift, false
		}
		if strings.TrimSpace(oldLine) == "" {
			continue
		}
		c, o := leadingIndent(fileLine), leadingIndent(oldLine)
		var got indentShift
		switch {
		case c == o:
		case strings.HasPrefix(c, o):
			got.add = c[len(o):]
		case strings.HasPrefix(o, c):
			got.remove = o[len(c):]
		default:
			return shift, false
		}
		if !decided {
			shift, decided = got, true
		} else if got != shift {
			return shift, false
		}
	}
	return shift, decided
}

func (s indentShift) apply(text string) string {
	if s == (indentShift{}) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if s.add != "" {
			lines[i] = s.add + line
			continue
		}
		indent := leadingIndent(line)
		n := 0
		for n < len(indent) && n < len(s.remove) && indent[n] == s.remove[n] {
			n++
		}
		lines[i] = line[n:]
	}
	return strings.Join(lines, "\n")
}

type indentMatch struct {
	start, end  int
	replacement string
}

// applyIndentShiftedEdit matches oldText as whole lines whose indentation is
// uniformly shifted relative to the file, and re-indents newText by the same
// shift so the file's real indentation is preserved.
func applyIndentShiftedEdit(content string, op editOp, pathArg string) (string, bool, error) {
	trailingNewline := strings.HasSuffix(op.oldText, "\n")
	oldLines := strings.Split(strings.TrimSuffix(op.oldText, "\n"), "\n")
	n := len(oldLines)

	hasText := false
	for _, l := range oldLines {
		if strings.TrimSpace(l) != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		return "", false, nil
	}

	lines := splitContentLines(content)

	var matches []indentMatch
	for i := 0; i+n <= len(lines); {
		window := lines[i : i+n]
		last := window[n-1]
		shift, ok := matchIndentShift(content, window, oldLines)
		if ok && trailingNewline && last.end >= len(content) {
			ok = false // old_string ends with a newline the file does not have here
		}
		if !ok {
			i++
			continue
		}
		end := last.end
		if trailingNewline {
			end++
		}
		start := window[0].start
		newText := preserveEditQuoteStyle(op.oldText, content[start:end], shift.apply(op.newText))
		matches = append(matches, indentMatch{start: start, end: end, replacement: newText})
		i += n
	}

	if len(matches) == 0 {
		return "", false, nil
	}
	if len(matches) > 1 && !op.replaceAll {
		return "", true, fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", len(matches), pathArg)
	}

	newContent := content
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		newContent = newContent[:m.start] + m.replacement + newContent[m.end:]
	}
	if newContent == content {
		return "", true, fmt.Errorf("no changes made to %s. The replacement produced identical content", pathArg)
	}
	return newContent, true, nil
}

const (
	missingContextLines = 3
	missingMaxLines     = 14
	minAnchorLineLength = 4
)

// missingOldStringError explains a failed match by showing the file lines that
// best resemble old_string, so the next attempt needs no extra read.
func missingOldStringError(content, oldText, pathArg string) error {
	prefix := fmt.Sprintf("could not find old_string in %s. Make sure it matches exactly (including whitespace and newlines).", pathArg)

	lines := splitContentLines(content)
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1] // no phantom empty line after the final newline
	}
	oldLines := strings.Split(strings.TrimSuffix(oldText, "\n"), "\n")

	scores := map[int]int{}
	for j, oldLine := range oldLines {
		key := lineKey(oldLine)
		if len(key) < minAnchorLineLength {
			continue
		}
		for i, cl := range lines {
			fileKey := lineKey(content[cl.start:cl.end])
			switch {
			case fileKey == key:
				scores[i-j] += len(key)
			case strings.Contains(fileKey, key):
				scores[i-j] += len(key) / 2
			}
		}
	}

	best, bestScore := 0, 0
	for start, score := range scores {
		if score > bestScore || (score == bestScore && start < best) {
			best, bestScore = start, score
		}
	}

	if bestScore == 0 {
		preview := content
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return fmt.Errorf("%s File starts with:\n%s", prefix, preview)
	}

	best = max(best, 0)
	from := max(0, best-missingContextLines)
	to := min(len(lines), best+len(oldLines)+missingContextLines)
	if to-from > missingMaxLines {
		to = from + missingMaxLines
	}

	var b strings.Builder
	for i := from; i < to; i++ {
		cl := lines[i]
		fmt.Fprintf(&b, "%d\t%s\n", i+1, content[cl.start:cl.end])
	}
	return fmt.Errorf("%s Closest match (lines %d-%d, shown as line<TAB>text):\n%s", prefix, from+1, to, strings.TrimSuffix(b.String(), "\n"))
}

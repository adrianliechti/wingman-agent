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
	var outputPrefix int64
	previousEnd := 0
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
		if op.replaceAll {
			outputPrefix += int64(start-previousEnd) + int64(len(newText))
			if err := checkEditSize(pathArg, outputPrefix); err != nil {
				return "", true, err
			}
			previousEnd = end
		}
		matches = append(matches, indentMatch{start: start, end: end, replacement: newText})
		if op.replaceAll {
			i += n
		} else {
			i++
		}
	}

	if len(matches) == 0 {
		return "", false, nil
	}
	if len(matches) > 1 && !op.replaceAll {
		return "", true, fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", len(matches), pathArg)
	}

	newContent, err := applyIndentMatches(content, matches, pathArg)
	if err != nil {
		return "", true, err
	}
	if newContent == content {
		return "", true, fmt.Errorf("no changes made to %s. The replacement produced identical content", pathArg)
	}
	return newContent, true, nil
}

type nonEmptyOldLine struct {
	rawIndex int
	text     string
}

type nonEmptyFileLine struct {
	lineIndex int
	cl        contentLine
}

// applyBlankLineTolerantEdit handles multi-line edits where old_string matches
// consecutive non-empty file lines but differs in blank lines (or in indentation
// of unchanged surrounding context lines that anchor the edit).
func applyBlankLineTolerantEdit(content string, op editOp, pathArg string) (string, bool, error) {
	trailingNewline := strings.HasSuffix(op.oldText, "\n")
	// Keep line terminators when comparing context and slicing replacements:
	// deleting a line must consume its newline, and EOF newline changes count.
	oldLines := strings.SplitAfter(op.oldText, "\n")
	if trailingNewline {
		oldLines = oldLines[:len(oldLines)-1]
	}
	newLines := strings.SplitAfter(op.newText, "\n")
	if strings.HasSuffix(op.newText, "\n") {
		newLines = newLines[:len(newLines)-1]
	}

	var oldNonEmpty []nonEmptyOldLine
	for idx, line := range oldLines {
		if strings.TrimSpace(line) != "" {
			oldNonEmpty = append(oldNonEmpty, nonEmptyOldLine{rawIndex: idx, text: strings.TrimSuffix(line, "\n")})
		}
	}
	if len(oldNonEmpty) < 2 {
		return "", false, nil
	}

	allLines := splitContentLines(content)
	var fileNonEmpty []nonEmptyFileLine
	for idx, cl := range allLines {
		if strings.TrimSpace(content[cl.start:cl.end]) != "" {
			fileNonEmpty = append(fileNonEmpty, nonEmptyFileLine{lineIndex: idx, cl: cl})
		}
	}
	if len(fileNonEmpty) < len(oldNonEmpty) {
		return "", false, nil
	}

	// Identify unchanged prefix/suffix lines between oldLines and newLines so that
	// if only an inner span changed, we preserve the file's unchanged context
	// lines (including its blank-line layout and indentation) verbatim.
	prefixRaw := 0
	for prefixRaw < len(oldLines) && prefixRaw < len(newLines) && oldLines[prefixRaw] == newLines[prefixRaw] {
		prefixRaw++
	}
	suffixRaw := 0
	for suffixRaw < len(oldLines)-prefixRaw && suffixRaw < len(newLines)-prefixRaw &&
		oldLines[len(oldLines)-1-suffixRaw] == newLines[len(newLines)-1-suffixRaw] {
		suffixRaw++
	}

	// Map raw unchanged prefix/suffix to indices in oldNonEmpty when the changed
	// span is bounded by non-empty lines in oldLines.
	changeFirstNonEmpty, changeLastNonEmpty := -1, -1
	for i, ol := range oldNonEmpty {
		if ol.rawIndex >= prefixRaw && ol.rawIndex < len(oldLines)-suffixRaw {
			if changeFirstNonEmpty == -1 {
				changeFirstNonEmpty = i
			}
			changeLastNonEmpty = i
		}
	}
	hasAnchoredInnerChange := (prefixRaw > 0 || suffixRaw > 0) &&
		changeFirstNonEmpty != -1 &&
		oldNonEmpty[changeFirstNonEmpty].rawIndex == prefixRaw &&
		oldNonEmpty[changeLastNonEmpty].rawIndex == len(oldLines)-suffixRaw-1

	m := len(oldNonEmpty)
	oldTextSlice := make([]string, m)
	for i, ol := range oldNonEmpty {
		oldTextSlice[i] = ol.text
	}

	var matches []indentMatch
	var outputPrefix int64
	previousEnd := 0
	for k := 0; k+m <= len(fileNonEmpty); {
		candidate := fileNonEmpty[k : k+m]
		keysMatch := true
		for j := 0; j < m; j++ {
			fileLine := content[candidate[j].cl.start:candidate[j].cl.end]
			if lineKey(fileLine) != lineKey(oldNonEmpty[j].text) {
				keysMatch = false
				break
			}
			if j > 0 {
				fileBlankGap := candidate[j].lineIndex - candidate[j-1].lineIndex - 1
				oldBlankGap := oldNonEmpty[j].rawIndex - oldNonEmpty[j-1].rawIndex - 1
				if fileBlankGap > max(2, oldBlankGap+2) {
					keysMatch = false
					break
				}
			}
		}
		if !keysMatch {
			k++
			continue
		}

		// Boundary blank lines are part of the replaced span, not extra context
		// to leave behind. Require them to exist so we cannot consume nearby text.
		firstLine := candidate[0].lineIndex - oldNonEmpty[0].rawIndex
		lastLine := candidate[m-1].lineIndex + len(oldLines) - oldNonEmpty[m-1].rawIndex - 1
		if firstLine < 0 || lastLine >= len(allLines) {
			k++
			continue
		}
		before := content[allLines[firstLine].start:candidate[0].cl.start]
		after := content[candidate[m-1].cl.end:allLines[lastLine].end]
		if strings.TrimSpace(before) != "" || strings.TrimSpace(after) != "" ||
			(trailingNewline && allLines[lastLine].end >= len(content)) {
			k++
			continue
		}

		windowCL := make([]contentLine, m)
		for j := range candidate {
			windowCL[j] = candidate[j].cl
		}
		shift, ok := matchIndentShift(content, windowCL, oldTextSlice)
		// Only unchanged anchors may differ from the changed span's indent shift.
		if !ok && hasAnchoredInnerChange {
			subCL := windowCL[changeFirstNonEmpty : changeLastNonEmpty+1]
			subOld := oldTextSlice[changeFirstNonEmpty : changeLastNonEmpty+1]
			shift, ok = matchIndentShift(content, subCL, subOld)
		}
		if !ok {
			k++
			continue
		}

		start, end := allLines[firstLine].start, allLines[lastLine].end
		oldText, newText := op.oldText, op.newText
		if trailingNewline {
			end++
		}
		if hasAnchoredInnerChange {
			start, end = candidate[changeFirstNonEmpty].cl.start, candidate[changeLastNonEmpty].cl.end
			oldText = strings.Join(oldLines[prefixRaw:len(oldLines)-suffixRaw], "")
			newText = strings.Join(newLines[prefixRaw:len(newLines)-suffixRaw], "")
			if strings.HasSuffix(oldText, "\n") {
				end++
			}
		}
		// Adjacent candidates can share boundary blank lines. Replace each byte once.
		if op.replaceAll && len(matches) > 0 && start < matches[len(matches)-1].end {
			k++
			continue
		}
		replacement := preserveEditQuoteStyle(oldText, content[start:end], shift.apply(newText))
		if op.replaceAll {
			outputPrefix += int64(start-previousEnd) + int64(len(replacement))
			if err := checkEditSize(pathArg, outputPrefix); err != nil {
				return "", true, err
			}
			previousEnd = end
		}
		matches = append(matches, indentMatch{start: start, end: end, replacement: replacement})
		if op.replaceAll {
			k += m
		} else {
			k++
		}
	}

	if len(matches) == 0 {
		return "", false, nil
	}
	if len(matches) > 1 && !op.replaceAll {
		return "", true, fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", len(matches), pathArg)
	}

	newContent, err := applyIndentMatches(content, matches, pathArg)
	if err != nil {
		return "", true, err
	}
	if newContent == content {
		return "", true, fmt.Errorf("no changes made to %s. The replacement produced identical content", pathArg)
	}
	return newContent, true, nil
}

func applyIndentMatches(content string, matches []indentMatch, path string) (string, error) {
	size := int64(len(content))
	for _, match := range matches {
		size += int64(len(match.replacement)) - int64(match.end-match.start)
	}
	if err := checkEditSize(path, size); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(int(size))
	pos := 0
	for _, match := range matches {
		b.WriteString(content[pos:match.start])
		b.WriteString(match.replacement)
		pos = match.end
	}
	b.WriteString(content[pos:])
	return b.String(), nil
}

const (
	missingContextLines = 3
	missingMaxLines     = 14
	minAnchorLineLength = 4
)

var anchorTokenPattern = regexp.MustCompile(`[A-Za-z0-9_./-]+`)

var ignoredAnchorTokens = map[string]bool{
	"true": true, "false": true, "null": true, "none": true,
	"func": true, "return": true, "case": true, "switch": true,
	"string": true, "bool": true, "int": true, "err": true, "error": true,
	"var": true, "const": true, "type": true, "struct": true, "interface": true,
}

func extractAnchorTokens(key string) []string {
	raw := anchorTokenPattern.FindAllString(key, -1)
	seen := make(map[string]bool, len(raw))
	var tokens []string
	for _, tok := range raw {
		if len(tok) < minAnchorLineLength || ignoredAnchorTokens[strings.ToLower(tok)] || seen[tok] {
			continue
		}
		seen[tok] = true
		tokens = append(tokens, tok)
	}
	return tokens
}

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
		tokens := extractAnchorTokens(key)
		for i, cl := range lines {
			fileKey := lineKey(content[cl.start:cl.end])
			switch {
			case fileKey == key:
				scores[i-j] += len(key) * 2
			case strings.Contains(fileKey, key):
				scores[i-j] += len(key)
			case len(fileKey) >= minAnchorLineLength && strings.Contains(key, fileKey):
				scores[i-j] += len(fileKey)
			default:
				for _, tok := range tokens {
					if strings.Contains(fileKey, tok) {
						scores[i-j] += len(tok)
					}
				}
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

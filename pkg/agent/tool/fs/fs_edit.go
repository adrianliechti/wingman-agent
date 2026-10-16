package fs

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func EditTool(root *os.Root, allowedWriteRoots ...string) tool.Tool {
	return batchEditTool(root, nil, allowedWriteRoots...)
}

type editOp struct {
	oldText    string
	newText    string
	replaceAll bool
}

func newEditOp(entry map[string]any) (editOp, error) {
	oldText, ok := entry["old_string"].(string)
	if !ok {
		return editOp{}, fmt.Errorf("old_string is required")
	}

	newText, ok := entry["new_string"].(string)
	if !ok {
		return editOp{}, fmt.Errorf("new_string is required")
	}
	if !utf8.ValidString(oldText) || !utf8.ValidString(newText) || strings.ContainsRune(oldText, 0) || strings.ContainsRune(newText, 0) {
		return editOp{}, fmt.Errorf("old_string and new_string must be UTF-8 text without NUL bytes")
	}

	if oldText == newText && oldText != "" {
		return editOp{}, fmt.Errorf("old_string and new_string are identical")
	}

	replaceAll := false
	if value, exists := entry["replace_all"]; exists {
		var ok bool
		replaceAll, ok = value.(bool)
		if !ok {
			return editOp{}, fmt.Errorf("replace_all must be a boolean")
		}
	}

	return editOp{
		oldText:    normalizeToLF(oldText),
		newText:    normalizeToLF(newText),
		replaceAll: replaceAll,
	}, nil
}

func applyEditOp(content string, op editOp, pathArg string) (string, error) {
	if err := checkEditSize(pathArg, int64(len(op.newText))); err != nil {
		return "", err
	}
	if op.oldText == "" {
		if content != "" {
			return "", fmt.Errorf("cannot create or replace empty file %s: file already has content", pathArg)
		}
		return op.newText, nil
	}

	newContent, found, err := applyMatchedEdit(content, op, pathArg)
	if found || err != nil {
		return newContent, err
	}

	// The model often copies from `read` output: the line-number gutter or a
	// uniformly shifted indentation would otherwise cost a retry turn.
	if oldText, newText, ok := stripReadLineNumbers(op.oldText, op.newText); ok {
		op.oldText, op.newText = oldText, newText
		newContent, found, err = applyMatchedEdit(content, op, pathArg)
		if found || err != nil {
			return newContent, err
		}
	}

	newContent, found, err = applyIndentShiftedEdit(content, op, pathArg)
	if found || err != nil {
		return newContent, err
	}

	newContent, found, err = applyBlankLineTolerantEdit(content, op, pathArg)
	if found || err != nil {
		return newContent, err
	}

	return "", missingOldStringError(content, op.oldText, pathArg)
}

// applyMatchedEdit applies op using exact, quote-normalized and whitespace-fuzzy
// matching. found is false (with a nil error) when old_string does not occur.
func applyMatchedEdit(content string, op editOp, pathArg string) (result string, found bool, err error) {
	actualOldText := findActualEditString(content, op.oldText)
	quoteNormalized := actualOldText != op.oldText

	matchResult := fuzzyFindText(content, actualOldText)

	if !matchResult.found {
		return "", false, nil
	}
	matched := content[matchResult.index : matchResult.index+matchResult.matchLength]
	actualNewText := preserveEditQuoteStyle(op.oldText, matched, op.newText)

	occurrences := countEditMatches(content, actualOldText, op.replaceAll)
	if matchResult.usedFuzzyMatch {
		occurrences = countEditMatches(normalizeForFuzzyMatch(content), normalizeForFuzzyMatch(actualOldText), op.replaceAll)
	} else if quoteNormalized {
		occurrences = countEditMatches(normalizeEditQuotes(content), normalizeEditQuotes(op.oldText), op.replaceAll)
	}

	if occurrences > 1 && !op.replaceAll {
		return "", true, fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", occurrences, pathArg)
	}

	var newContent string

	if op.replaceAll {
		if matchResult.usedFuzzyMatch || quoteNormalized {
			newContent, err = replaceAllMatches(content, op, pathArg)
			if err != nil {
				return "", true, err
			}
			if newContent == content {
				return "", true, fmt.Errorf("replace_all made no progress in %s. Use an exact old_string or a replacement that changes the matched text", pathArg)
			}
		} else {
			if err := checkEditSize(pathArg, int64(len(content))+int64(occurrences)*int64(len(actualNewText)-len(actualOldText))); err != nil {
				return "", true, err
			}
			newContent = strings.ReplaceAll(content, actualOldText, actualNewText)
		}
	} else {
		if err := checkEditSize(pathArg, int64(len(content)-matchResult.matchLength)+int64(len(actualNewText))); err != nil {
			return "", true, err
		}
		newContent = content[:matchResult.index] + actualNewText + content[matchResult.index+matchResult.matchLength:]
	}

	if content == newContent {
		return "", true, fmt.Errorf("no changes made to %s. The replacement produced identical content", pathArg)
	}

	return newContent, true, nil
}

// replaceAllMatches replaces every quote-normalized or whitespace-fuzzy match in
// a single left-to-right pass. Replaced text is never rescanned, so a replacement
// that contains old_string cannot loop or be rejected.
func replaceAllMatches(content string, op editOp, pathArg string) (string, error) {
	normalized, offsets := normalizeForFuzzyMatchWithMap(content)
	old := normalizeForFuzzyMatch(op.oldText)
	var b strings.Builder
	pos, normalizedPos := 0, 0
	for old != "" && normalizedPos < len(normalized) {
		index := strings.Index(normalized[normalizedPos:], old)
		if index < 0 {
			break
		}
		index += normalizedPos
		mr := originalFuzzyMatch(content, normalized, offsets, index, len(old))
		end := mr.index + mr.matchLength
		// The previous match may already have consumed trailing whitespace
		// before this newline. The shared offset map still includes those bytes.
		start := max(pos, mr.index)
		replacement := preserveEditQuoteStyle(op.oldText, content[start:end], op.newText)
		if err := checkEditSize(pathArg, int64(b.Len()+start-pos)+int64(len(replacement))); err != nil {
			return "", err
		}
		b.WriteString(content[pos:start])
		b.WriteString(replacement)
		pos, normalizedPos = end, index+len(old)
	}
	if err := checkEditSize(pathArg, int64(b.Len()+len(content)-pos)); err != nil {
		return "", err
	}
	b.WriteString(content[pos:])
	return b.String(), nil
}

func checkEditSize(path string, size int64) error {
	if size > MaxEditFileBytes {
		return fmt.Errorf("file %s would exceed %d bytes; edits are capped at %d bytes", path, MaxEditFileBytes, MaxEditFileBytes)
	}
	return nil
}

// A unique match must also exclude overlapping occurrences. replace_all uses
// the usual left-to-right, non-overlapping replacement semantics.
func countEditMatches(content, old string, replaceAll bool) int {
	if replaceAll {
		return strings.Count(content, old)
	}
	count := 0
	for old != "" {
		index := strings.Index(content, old)
		if index < 0 {
			break
		}
		count++
		content = content[index+1:]
	}
	return count
}

func findActualEditString(content, oldText string) string {
	if strings.Contains(content, oldText) {
		return oldText
	}

	normContent, offsets := normalizeEditQuotesWithMap(content)
	normOld := normalizeEditQuotes(oldText)

	idx := strings.Index(normContent, normOld)
	if idx == -1 {
		return oldText
	}

	return content[offsets[idx]:offsets[idx+len(normOld)]]
}

var editQuoteReplacer = strings.NewReplacer(
	"‘", "'", "’", "'",
	"“", "\"", "”", "\"",
)

func normalizeEditQuotes(text string) string {
	return editQuoteReplacer.Replace(text)
}

func normalizeEditQuotesWithMap(text string) (string, []int) {
	var b strings.Builder
	b.Grow(len(text))
	offsets := make([]int, 0, len(text)+1)
	offsets = append(offsets, 0)

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '‘', '’':
			b.WriteByte('\'')
			offsets = append(offsets, i+size)
		case '“', '”':
			b.WriteByte('"')
			offsets = append(offsets, i+size)
		default:
			b.WriteString(text[i : i+size])
			for j := 1; j <= size; j++ {
				offsets = append(offsets, i+j)
			}
		}
		i += size
	}
	return b.String(), offsets
}

func preserveEditQuoteStyle(oldText, actualOldText, newText string) string {
	if oldText == actualOldText {
		return newText
	}

	result := newText
	if strings.ContainsAny(actualOldText, "“”") && !strings.ContainsAny(oldText, "“”") && !strings.Contains(actualOldText, "\"") {
		result = applyCurlyDoubleQuotes(result)
	}
	if strings.ContainsAny(actualOldText, "‘’") && !strings.ContainsAny(oldText, "‘’") && !strings.Contains(actualOldText, "'") {
		result = applyCurlySingleQuotes(result)
	}
	return result
}

func applyCurlyDoubleQuotes(text string) string {
	var b strings.Builder
	for i, r := range text {
		if r != '"' {
			b.WriteRune(r)
			continue
		}
		if isOpeningQuoteContext(text, i) {
			b.WriteString("“")
		} else {
			b.WriteString("”")
		}
	}
	return b.String()
}

func applyCurlySingleQuotes(text string) string {
	runes := []rune(text)
	var b strings.Builder
	for i, r := range runes {
		if r != '\'' {
			b.WriteRune(r)
			continue
		}

		if i > 0 && i < len(runes)-1 && isLetter(runes[i-1]) && isLetter(runes[i+1]) {
			b.WriteString("’")
			continue
		}

		if isOpeningQuoteContextRunes(runes, i) {
			b.WriteString("‘")
		} else {
			b.WriteString("’")
		}
	}
	return b.String()
}

func isOpeningQuoteContext(text string, byteIndex int) bool {
	preceding := []rune(text[:byteIndex])
	return isOpeningQuoteContextRunes(preceding, len(preceding))
}

func isOpeningQuoteContextRunes(runes []rune, index int) bool {
	if index == 0 {
		return true
	}
	prev := runes[index-1]
	switch prev {
	case ' ', '\t', '\n', '\r', '(', '[', '{':
		return true
	default:
		return false
	}
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

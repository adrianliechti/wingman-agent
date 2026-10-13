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

	if oldText == newText && oldText != "" {
		return editOp{}, fmt.Errorf("old_string and new_string are identical")
	}

	replaceAll, _ := entry["replace_all"].(bool)

	return editOp{
		oldText:    normalizeToLF(oldText),
		newText:    normalizeToLF(newText),
		replaceAll: replaceAll,
	}, nil
}

func applyEditOp(content string, op editOp, pathArg string) (string, error) {
	if op.oldText == "" {
		if strings.TrimSpace(content) != "" {
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

	return "", missingOldStringError(content, op.oldText, pathArg)
}

// applyMatchedEdit applies op using exact, quote-normalized and whitespace-fuzzy
// matching. found is false (with a nil error) when old_string does not occur.
func applyMatchedEdit(content string, op editOp, pathArg string) (result string, found bool, err error) {
	actualOldText := findActualEditString(content, op.oldText)
	actualNewText := preserveEditQuoteStyle(op.oldText, actualOldText, op.newText)
	quoteNormalized := actualOldText != op.oldText

	matchResult := fuzzyFindText(content, actualOldText)

	if !matchResult.found {
		return "", false, nil
	}

	occurrences := strings.Count(content, actualOldText)
	if matchResult.usedFuzzyMatch {
		occurrences = strings.Count(normalizeForFuzzyMatch(content), normalizeForFuzzyMatch(actualOldText))
	} else if quoteNormalized {
		occurrences = strings.Count(normalizeEditQuotes(content), normalizeEditQuotes(op.oldText))
	}

	if occurrences > 1 && !op.replaceAll {
		return "", true, fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", occurrences, pathArg)
	}

	var newContent string

	if op.replaceAll {
		if matchResult.usedFuzzyMatch || quoteNormalized {
			newContent = replaceAllMatches(content, op)
			if newContent == content {
				return "", true, fmt.Errorf("replace_all made no progress in %s. Use an exact old_string or a replacement that changes the matched text", pathArg)
			}
		} else {
			newContent = strings.ReplaceAll(content, actualOldText, actualNewText)
		}
	} else {
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
func replaceAllMatches(content string, op editOp) string {
	var b strings.Builder
	pos := 0
	for pos < len(content) {
		rest := content[pos:]
		mr := fuzzyFindText(rest, op.oldText)
		if !mr.found || mr.matchLength == 0 {
			break
		}
		matched := rest[mr.index : mr.index+mr.matchLength]
		b.WriteString(rest[:mr.index])
		b.WriteString(preserveEditQuoteStyle(op.oldText, matched, op.newText))
		pos += mr.index + mr.matchLength
	}
	b.WriteString(content[pos:])
	return b.String()
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

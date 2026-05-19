package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func EditTool(root *os.Root, allowedWriteRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "edit",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Perform exact string replacements in files. Fails if `old_string` is not unique unless `replace_all=true`.",
			"- Read the file first so `old_string` matches current text.",
			"- Prefer this for existing files; it produces smaller, reviewable diffs. Use `write` only for new files or complete rewrites.",
			"- `read` line prefixes (`42\\t...`) are not file content. Match only text after the prefix, preserving indentation.",
			"- Use the smallest unique `old_string` — usually 2-4 adjacent lines. If matching fails, re-read the relevant slice.",
			"- To create a new file or replace an empty file, use empty `old_string`; non-empty files reject empty `old_string`.",
			"- Use `replace_all=true` for intentional file-wide renames/replacements.",
			"- Do not insert emoji unless asked.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path to modify."},
				"old_string":  map[string]any{"type": "string", "description": "Exact text to replace. Must be unique unless replace_all=true. Use an empty string only to create a new file or replace an empty file."},
				"new_string":  map[string]any{"type": "string", "description": "Replacement text. Must differ from old_string."},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences of old_string. Defaults to false."},
			},
			"required":             []string{"path", "old_string", "new_string"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("path is required")
			}

			workingDir := root.Name()
			target, err := resolveFileTarget(pathArg, workingDir, allowedWriteRoots, "edit file")
			if err != nil {
				return "", err
			}

			oldText, ok := args["old_string"].(string)

			if !ok {
				return "", fmt.Errorf("old_string is required")
			}

			newText, ok := args["new_string"].(string)

			if !ok {
				return "", fmt.Errorf("new_string is required")
			}

			if oldText == newText {
				return "", fmt.Errorf("no changes made to %s. old_string and new_string are identical", pathArg)
			}

			info, err := statFileTarget(root, target)
			exists := err == nil
			switch {
			case exists:
				if info.IsDir() {
					return "", fmt.Errorf("cannot edit file: path %q is a directory", pathArg)
				}
			case !os.IsNotExist(err):
				return "", fmt.Errorf("stat file %q: %w", pathArg, err)
			case oldText != "":
				return "", fmt.Errorf("cannot edit %s: file does not exist", pathArg)
			}

			var contentBytes []byte
			if exists {
				contentBytes, err = readFileTarget(root, target)
				if err != nil {
					return "", fmt.Errorf("read file %q: %w", pathArg, err)
				}
			}

			if len(contentBytes) > MaxEditFileBytes {
				return "", fmt.Errorf("file %s is %d bytes; edits are capped at %d bytes — use `write` for full rewrites or narrow the change", pathArg, len(contentBytes), MaxEditFileBytes)
			}

			bom, content := stripBom(string(contentBytes))
			originalEnding := detectLineEnding(content)
			normalizedContent := normalizeToLF(content)
			normalizedOldText := normalizeToLF(oldText)
			normalizedNewText := normalizeToLF(newText)

			if oldText == "" {
				if strings.TrimSpace(normalizedContent) != "" {
					return "", fmt.Errorf("cannot create or replace empty file %s: file already has content", pathArg)
				}

				finalContent := bom + restoreLineEndings(normalizedNewText, originalEnding)
				if err := writeFileTarget(root, target, finalContent); err != nil {
					return "", fmt.Errorf("write file %q: %w", pathArg, err)
				}

				diff := generateDiffString("", normalizedNewText)
				return fmt.Sprintf("Successfully replaced text in %s.\n\n%s", pathArg, diff), nil
			}

			replaceAll := false
			if ra, ok := args["replace_all"].(bool); ok {
				replaceAll = ra
			}

			actualOldText := findActualEditString(normalizedContent, normalizedOldText)
			actualNewText := preserveEditQuoteStyle(normalizedOldText, actualOldText, normalizedNewText)

			matchResult := fuzzyFindText(normalizedContent, actualOldText)

			if !matchResult.found {
				preview := normalizedContent
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				return "", fmt.Errorf("could not find old_string in %s. Make sure it matches exactly (including whitespace and newlines). File starts with:\n%s", pathArg, preview)
			}

			fuzzyContent := normalizeForFuzzyMatch(normalizedContent)
			fuzzyOldText := normalizeForFuzzyMatch(actualOldText)
			occurrences := strings.Count(fuzzyContent, fuzzyOldText)

			if occurrences > 1 && !replaceAll {
				return "", fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", occurrences, pathArg)
			}

			var newContent string

			if replaceAll {
				if matchResult.usedFuzzyMatch {
					if strings.Contains(normalizeForFuzzyMatch(actualNewText), fuzzyOldText) {
						return "", fmt.Errorf("replace_all made no progress in %s. Use an exact old_string or a replacement that changes the matched text", pathArg)
					}
					newContent = normalizedContent
					for {
						mr := fuzzyFindText(newContent, actualOldText)
						if !mr.found {
							break
						}
						next := newContent[:mr.index] + actualNewText + newContent[mr.index+mr.matchLength:]
						if next == newContent {
							return "", fmt.Errorf("replace_all made no progress in %s. Use an exact old_string or a replacement that changes the matched text", pathArg)
						}
						newContent = next
					}
				} else {
					newContent = strings.ReplaceAll(normalizedContent, actualOldText, actualNewText)
				}
			} else {
				newContent = normalizedContent[:matchResult.index] + actualNewText + normalizedContent[matchResult.index+matchResult.matchLength:]
			}

			if normalizedContent == newContent {
				return "", fmt.Errorf("no changes made to %s. The replacement produced identical content", pathArg)
			}

			finalContent := bom + restoreLineEndings(newContent, originalEnding)

			if err := writeFileTarget(root, target, finalContent); err != nil {
				return "", fmt.Errorf("write file %q: %w", pathArg, err)
			}

			diff := generateDiffString(normalizedContent, newContent)

			return fmt.Sprintf("Successfully replaced text in %s.\n\n%s", pathArg, diff), nil
		},
	}
}

func writeRootFile(root *os.Root, path, content string) (err error) {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := root.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	outFile, err := root.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := outFile.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("failed to close file: %w", closeErr)
		}
	}()

	if _, err := outFile.WriteString(content); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// findActualEditString returns the slice of `content` that matches `oldText`
// up to curly/ASCII quote normalization. When the curly form differs from the
// caller-supplied straight form, the original-content slice is returned so
// preserveEditQuoteStyle can mirror the file's quote style back into newText.
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
	"‘", "'", "’", "'", // ‘ ’
	"“", "\"", "”", "\"", // “ ”
)

func normalizeEditQuotes(text string) string {
	return editQuoteReplacer.Replace(text)
}

// normalizeEditQuotesWithMap returns the quote-normalized text alongside a
// byte-offset map: offsets[i] is the byte index in the original text that
// corresponds to byte index i in the normalized text. Length is
// len(normalized)+1 so callers can take the end offset of a match.
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
	if strings.ContainsAny(actualOldText, "“”") {
		result = applyCurlyDoubleQuotes(result)
	}
	if strings.ContainsAny(actualOldText, "‘’") {
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

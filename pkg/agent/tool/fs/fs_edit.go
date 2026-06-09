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
			"Performs exact string replacements in files.",
			"- You must `read` the file at least once in this conversation before editing it.",
			"- Preserve indentation exactly as it appears after the `read` line-number prefix (`42\\t...`). Never include the prefix in old_string/new_string.",
			"- The edit will FAIL if `old_string` is not unique in the file. Provide more surrounding context to make it unique, or set `replace_all=true` to change every instance.",
			"- Use the smallest old_string that's clearly unique — usually 2-4 adjacent lines is sufficient.",
			"- Prefer `write` for new files. An empty `old_string` also creates a new file (or replaces an empty one); non-empty files reject empty `old_string`.",
			"- Use `replace_all` for renaming a symbol across a file or other intentional file-wide replacements.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path":   map[string]any{"type": "string", "description": "The absolute path to the file to modify."},
				"old_string":  map[string]any{"type": "string", "description": "The text to replace. Must be unique unless replace_all=true. Use an empty string only to create a new file or replace an empty file."},
				"new_string":  map[string]any{"type": "string", "description": "The text to replace it with (must be different from old_string)."},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences of old_string (default false).", "default": false},
			},
			"required":             []string{"file_path", "old_string", "new_string"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["file_path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("file_path is required")
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

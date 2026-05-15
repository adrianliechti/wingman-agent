package fs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func EditTool(root *os.Root) tool.Tool {
	return tool.Tool{
		Name:   "edit",
		Effect: tool.StaticEffect(tool.EffectMutates),

		Description: strings.Join([]string{
			"Replace `old_string` with `new_string` in a file. Fails if `old_string` is not unique unless `replace_all=true`.",
			"- Read the file first in this conversation so old_string matches current text.",
			"- Line-number prefixes (`     42\\t…`) shown by `read` are NOT part of file content. Match only the text AFTER the prefix, preserving exact indentation (tabs vs spaces).",
			"- Use the smallest uniquely-identifying old_string — usually 2-4 adjacent lines. If matching fails, re-read the relevant slice rather than guessing.",
			"- Do not insert emoji unless the user asked for them.",
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path."},
				"old_string":  map[string]any{"type": "string", "description": "Exact text to find. Must be unique unless replace_all=true."},
				"new_string":  map[string]any{"type": "string", "description": "Replacement text. Must differ from old_string."},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence."},
			},
			"required": []string{"path", "old_string", "new_string"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("path is required")
			}

			workingDir := root.Name()

			normalizedPath, err := ensurePathInWorkspace(pathArg, workingDir, "edit file")

			if err != nil {
				return "", err
			}

			oldText, ok := args["old_string"].(string)

			if !ok || oldText == "" {
				return "", fmt.Errorf("old_string is required")
			}

			newText, ok := args["new_string"].(string)

			if !ok {
				return "", fmt.Errorf("new_string is required")
			}

			if oldText == newText {
				return "", fmt.Errorf("no changes made to %s. old_string and new_string are identical", pathArg)
			}

			contentBytes, err := root.ReadFile(normalizedPath)

			if err != nil {
				return "", pathError("read file", pathArg, normalizedPath, workingDir, err)
			}

			if len(contentBytes) > MaxEditFileBytes {
				return "", fmt.Errorf("file %s is %d bytes; edits are capped at %d bytes — use `write` for full rewrites or narrow the change", pathArg, len(contentBytes), MaxEditFileBytes)
			}

			bom, content := stripBom(string(contentBytes))
			originalEnding := detectLineEnding(content)
			normalizedContent := normalizeToLF(content)
			normalizedOldText := normalizeToLF(oldText)
			normalizedNewText := normalizeToLF(newText)

			replaceAll := false
			if ra, ok := args["replace_all"].(bool); ok {
				replaceAll = ra
			}

			matchResult := fuzzyFindText(normalizedContent, normalizedOldText)

			if !matchResult.found {
				preview := normalizedContent
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				return "", fmt.Errorf("could not find old_string in %s. Make sure it matches exactly (including whitespace and newlines). File starts with:\n%s", pathArg, preview)
			}

			fuzzyContent := normalizeForFuzzyMatch(normalizedContent)
			fuzzyOldText := normalizeForFuzzyMatch(normalizedOldText)
			occurrences := strings.Count(fuzzyContent, fuzzyOldText)

			if occurrences > 1 && !replaceAll {
				return "", fmt.Errorf("found %d occurrences of the text in %s. The text must be unique — provide more context to make it unique, or set replace_all=true to replace all occurrences", occurrences, pathArg)
			}

			baseContent := matchResult.contentForReplacement
			var newContent string

			if replaceAll {
				if matchResult.usedFuzzyMatch {
					if strings.Contains(normalizeForFuzzyMatch(normalizedNewText), fuzzyOldText) {
						return "", fmt.Errorf("replace_all made no progress in %s. Use an exact old_string or a replacement that changes the matched text", pathArg)
					}
					newContent = baseContent
					for {
						mr := fuzzyFindText(newContent, normalizedOldText)
						if !mr.found {
							break
						}
						nextContent := mr.contentForReplacement[:mr.index] + normalizedNewText + mr.contentForReplacement[mr.index+mr.matchLength:]
						if nextContent == newContent {
							return "", fmt.Errorf("replace_all made no progress in %s. Use an exact old_string or a replacement that changes the matched text", pathArg)
						}
						newContent = nextContent
					}
				} else {
					newContent = strings.ReplaceAll(baseContent, normalizedOldText, normalizedNewText)
				}
			} else {
				newContent = baseContent[:matchResult.index] + normalizedNewText + baseContent[matchResult.index+matchResult.matchLength:]
			}

			if baseContent == newContent {
				return "", fmt.Errorf("no changes made to %s. The replacement produced identical content", pathArg)
			}

			finalContent := bom + restoreLineEndings(newContent, originalEnding)

			outFile, err := root.Create(normalizedPath)

			if err != nil {
				return "", pathError("write file", pathArg, normalizedPath, workingDir, err)
			}
			if _, err := outFile.WriteString(finalContent); err != nil {
				outFile.Close()
				return "", fmt.Errorf("failed to write file: %w", err)
			}

			if err := outFile.Close(); err != nil {
				return "", fmt.Errorf("failed to close file: %w", err)
			}

			diff := generateDiffString(baseContent, newContent)

			result := fmt.Sprintf("Successfully replaced text in %s.\n\n%s", pathArg, diff)

			return result, nil
		},
	}
}

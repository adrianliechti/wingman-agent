package fetch

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const (
	defaultExcerptLines = 20
	maxFindRunes        = 512
)

type pageView struct {
	offset int
	limit  int
	find   *regexp.Regexp
}

func parsePageView(args map[string]any) (pageView, error) {
	offset, _, err := tool.PositiveIntArg(args, "offset")
	if err != nil {
		return pageView{}, err
	}
	limit, _, err := tool.PositiveIntArg(args, "limit")
	if err != nil {
		return pageView{}, err
	}
	v := pageView{offset: offset, limit: limit}
	if raw, present := args["find"]; present {
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" || strings.ContainsAny(s, "\r\n") || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxFindRunes {
			return pageView{}, fmt.Errorf("find must be a single line of text of up to %d characters", maxFindRunes)
		}
		v.find = regexp.MustCompile("(?i)" + regexp.QuoteMeta(s))
	}
	return v, nil
}

func (v pageView) render(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "[no readable text content in the fetched response]", false
	}
	// Keep the original unnumbered representation for ordinary short fetches.
	if v == (pageView{}) && len(trimmed) <= maxOutputBytes {
		return trimmed, false
	}
	content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	// A final line terminator does not create an extra line.
	content = strings.TrimSuffix(content, "\n")
	if v == (pageView{}) {
		if len(content) <= maxOutputBytes {
			return strings.TrimSpace(content), false
		}
		preview := text.HeadLines(content, maxOutputBytes)
		next := "Use find to locate a passage in a long line."
		if content[len(preview)] == '\n' {
			next = fmt.Sprintf("Use offset=%d to continue.", strings.Count(preview, "\n")+2)
		}
		return fmt.Sprintf("%s\n\n[truncated at %dKB] %s", preview, maxOutputBytes/1024, next), true
	}

	total := strings.Count(content, "\n") + 1
	start := max(1, v.offset)
	if start > total {
		return fmt.Sprintf("No line %d in the fetched text (%d lines).", start, total), false
	}
	for range start - 1 {
		_, content, _ = strings.Cut(content, "\n")
	}
	var match []int
	if v.find != nil {
		match = v.find.FindStringIndex(content)
		if match == nil {
			return fmt.Sprintf("No match in the fetched text at or after line %d.", start), false
		}
		before := content[:match[0]]
		start += strings.Count(before, "\n")
		lineStart := strings.LastIndexByte(before, '\n') + 1
		content = content[lineStart:]
		match[0] -= lineStart
		match[1] -= lineStart
	}
	limit := v.limit
	if limit == 0 {
		limit = defaultExcerptLines
	}
	var out strings.Builder
	lineNumber := start
	shortened := false
	for line := range strings.SplitSeq(content, "\n") {
		prefix := strconv.Itoa(lineNumber) + "\t"
		available := maxOutputBytes - out.Len() - len(prefix) - 1
		if len(line) > available {
			if out.Len() != 0 {
				break // Offer the entire next line on a fresh page.
			}
			shortened = true
			if match != nil && match[1] > available-len("…") {
				// A long paragraph may contain the match far beyond its preview.
				leading := text.TailBytes(line[:match[0]], 160)
				line = line[match[0]-len(leading):]
				prefix += "…"
				available -= len("…")
			}
			if len(line) > available {
				line = text.HeadBytes(line, available-len("…")) + "…"
			}
		}
		out.WriteString(prefix)
		out.WriteString(line)
		out.WriteByte('\n')
		lineNumber++
		if lineNumber-start == limit {
			break
		}
	}
	fmt.Fprintf(&out, "\n[Lines %d-%d of %d.", start, lineNumber-1, total)
	if shortened {
		out.WriteString(" Long line shortened.")
	}
	if lineNumber <= total {
		fmt.Fprintf(&out, " Use offset=%d to continue.", lineNumber)
	}
	out.WriteByte(']')
	return out.String(), shortened || start > 1 || lineNumber <= total
}

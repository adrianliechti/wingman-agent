package lsp

import (
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type resultPage struct{ offset, limit int }

func parsePage(args map[string]any) (resultPage, error) {
	p := resultPage{limit: 100}
	for _, key := range []string{"offset", "limit"} {
		if raw, present := args[key]; present {
			n, ok := tool.IntValue(raw)
			if !ok || n < 0 || key == "limit" && (n < 1 || n > 100) {
				return p, fmt.Errorf("%s must be an integer: offset >= 0, limit between 1 and 100", key)
			}
			if key == "offset" {
				p.offset = n
			} else {
				p.limit = n
			}
		}
	}
	return p, nil
}

func selectedPage(pages []resultPage) resultPage {
	if len(pages) > 0 {
		return pages[0]
	}
	return resultPage{limit: 100}
}

func pageSlice[T any](values []T, p resultPage) []T {
	start := min(p.offset, len(values))
	return values[start : start+min(p.limit, len(values)-start)]
}

func pageNote(total int, p resultPage, noun string) string {
	start := min(p.offset, total)
	end := start + min(p.limit, total-start)
	if start == 0 && end == total {
		return ""
	}
	note := fmt.Sprintf("\nShowing %d–%d of %d %s.", min(start+1, end), end, total, noun)
	if start == 0 {
		note = fmt.Sprintf("\n... showing first %d of %d %s.", end, total, noun)
	}
	if end < total {
		note += fmt.Sprintf(" Continue with offset=%d, limit=%d and the same query; results may change if files change.", end, p.limit)
	}
	return note + "\n"
}

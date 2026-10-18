package shell

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func confirmIfDangerous(ctx context.Context, elicit *tool.Elicitation, kind, text string, dangerous bool) error {
	if !dangerous {
		return nil
	}
	if elicit == nil || (elicit.Confirm == nil && elicit.Review == nil) {
		return fmt.Errorf("command requires approval, which is unavailable here")
	}

	verdict, err := elicit.Approve(ctx, tool.Approval{
		Message: "❯ " + approvalDisplayText(text),
		Action:  map[string]any{"type": kind, "text": text},
	})

	if err != nil {
		return fmt.Errorf("failed to get approval: %w", err)
	}

	if err := verdict.Err("command execution denied by user"); err != nil {
		return err
	}

	return nil
}

// approvalDisplayText makes characters that can move, erase, or reorder a
// terminal prompt visible without changing ordinary command formatting.
func approvalDisplayText(text string) string {
	var out strings.Builder
	for _, r := range text {
		switch r {
		case '\n':
			out.WriteRune(r)
		case '\t':
			out.WriteString(`\t`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
				fmt.Fprintf(&out, `\u{%04X}`, r)
				continue
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}

// ConfirmCommand checks a script with the interactive shell classifier and gate.
func ConfirmCommand(ctx context.Context, command, workdir string, elicit *tool.Elicitation, options ...*Options) error {
	opts := &Options{}
	if len(options) > 0 && options[0] != nil {
		opts = options[0]
	}
	return confirmExecution(ctx, elicit, command, workdir, workdir, "use_default", "", opts)
}

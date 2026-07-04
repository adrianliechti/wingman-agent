package fs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const maxImageBytes = 10 * 1024 * 1024

var imageMimeTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

func ImageTool(root *os.Root, allowedReadRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "view_image",
		Effect: tool.StaticEffect(tool.EffectReadOnly),

		Description: strings.Join([]string{
			"View an image file: it is attached to the conversation so you can actually see it.",
			"- Use for screenshots, UI mockups, diagrams, and other visual assets — e.g. take a screenshot via shell, then view it to verify a UI change.",
			fmt.Sprintf("- Supported: PNG, JPEG, GIF, WebP. Max %dMB.", maxImageBytes/(1024*1024)),
		}, "\n"),

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string", "description": "Path to the image file."},
			},
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pathArg, ok := args["file_path"].(string)

			if !ok || pathArg == "" {
				return "", fmt.Errorf("file_path is required")
			}

			target, err := resolveFileTarget(pathArg, root.Name(), allowedReadRoots, "view image")
			if err != nil {
				return "", err
			}

			info, err := statFileTarget(root, target)
			if err != nil {
				return "", fmt.Errorf("stat file %q: %w", pathArg, err)
			}
			if info.IsDir() {
				return "", fmt.Errorf("cannot view image: path %q is a directory", pathArg)
			}
			if info.Size() > maxImageBytes {
				return "", fmt.Errorf("cannot view image: %q is %dMB (max %dMB)", pathArg, info.Size()/(1024*1024), maxImageBytes/(1024*1024))
			}

			content, err := readFileTarget(root, target)
			if err != nil {
				return "", fmt.Errorf("read file %q: %w", pathArg, err)
			}

			mime := imageMimeTypes[strings.ToLower(filepath.Ext(pathArg))]
			if mime == "" {
				mime = http.DetectContentType(content)
			}
			if !strings.HasPrefix(mime, "image/") || mime == "image/svg+xml" {
				return "", fmt.Errorf("cannot view image: %q is not a supported image format (detected %s)", pathArg, mime)
			}

			return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(content), nil
		},
	}
}

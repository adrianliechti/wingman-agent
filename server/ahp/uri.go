package ahp

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
)

func ValidSessionURI(resource string) bool {
	return ValidURI(resource, "ahp-session")
}

func ValidURI(resource, scheme string) bool {
	u, err := url.Parse(resource)
	return err == nil && u.Scheme == scheme && u.Host == "" && strings.HasPrefix(u.Path, "/") && len(u.Path) > 1 && u.RawQuery == "" && u.Fragment == "" && len(resource) < 2048
}

func ChatURI(resource string) string {
	return strings.Replace(resource, "ahp-session:", "ahp-chat:", 1)
}

func SessionURI(chat string) string {
	return strings.Replace(chat, "ahp-chat:", "ahp-session:", 1)
}

// Terminal URI schemes are server-defined. Accept client-chosen schemes
// (including VS Code's agenthost-terminal), but reserve other ahp-* namespaces
// for their protocol channels. Never rewrite the terminal's subscription identity.
func ValidTerminalURI(resource string) bool {
	u, err := url.Parse(resource)
	if err != nil || u.Scheme == "" || strings.HasPrefix(u.Scheme, "ahp-") && u.Scheme != "ahp-terminal" {
		return false
	}
	return ValidURI(resource, u.Scheme)
}

func FilePath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" && u.Host != "localhost" || u.RawQuery != "" || u.Fragment != "" {
		return "", InvalidParams("local file URI required")
	}
	path, _ := fileuri.Path(uri)
	if !filepath.IsAbs(path) {
		return "", InvalidParams("absolute file URI required")
	}
	return path, nil
}

package code

import (
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

// RemoveLegacyTools deletes the language servers and debuggers earlier versions installed.
func RemoveLegacyTools() error {
	root, err := layout.WingmanPath("tools")
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

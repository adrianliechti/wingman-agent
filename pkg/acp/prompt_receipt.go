package acp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rogpeppe/go-internal/lockedfile"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

// Each key has a separate OS lock so independent sessions and submissions
// can proceed concurrently. Keep the raw key and prompt out of the filesystem.
func promptReceiptPath(agent, session, key string) (string, error) {
	hash := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
	return layout.WingmanPath("acp", "receipts", hash(agent), hash(session), hash(key))
}

// A completed receipt is immutable. A concurrent writer or an incomplete
// receipt always falls back to the locked lookup before starting any work.
func recordedPrompt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var id string
	if json.Unmarshal(data, &id) != nil {
		return ""
	}
	return id
}

func openPromptReceipt(path string) (*lockedfile.File, string, error) {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, "", err
	}
	f, err := lockedfile.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(f)
	var messageID string
	if err == nil && len(data) > 0 {
		err = json.Unmarshal(data, &messageID)
		if err == nil && messageID == "" {
			err = fmt.Errorf("prompt insertion outcome is unknown")
		}
	}
	if err != nil || messageID != "" {
		f.Close()
		return nil, messageID, err
	}
	// If the process exits during insertion, a retry must not run the work twice.
	if _, err = f.WriteString("null"); err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return nil, "", err
	}
	return f, "", nil
}

func finishPromptReceipt(f *lockedfile.File, messageID string) error {
	if f == nil {
		return nil
	}
	defer f.Close()
	var data []byte
	if messageID != "" {
		data, _ = json.Marshal(messageID)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return err
	}
	return f.Sync()
}

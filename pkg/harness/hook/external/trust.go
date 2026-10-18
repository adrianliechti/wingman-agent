package external

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Fingerprint hashes the hook declarations together with their source identity.
func (c *Config) Fingerprint(identity ...string) (string, error) {
	declarations, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, part := range append(identity, string(declarations)) {
		writeHashPart(hash, part)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// ContentHash fingerprints a plugin bundle, skipping Python build artifacts like Codex.
func ContentHash(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("plugin root is not a directory")
	}
	hash := sha256.New()
	writeHashPart(hash, "wingman-plugin-bundle-v1")
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if path != root && (name == "__pycache__" || name == ".venv" || strings.HasSuffix(name, ".pyc") || strings.HasSuffix(name, ".egg-info")) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		writeHashPart(hash, filepath.ToSlash(relative))
		switch {
		case entry.IsDir():
			writeHashPart(hash, "directory")
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			writeHashPart(hash, "symlink")
			writeHashPart(hash, target)
		case entry.Type().IsRegular():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			writeHashPart(hash, "file")
			_ = binary.Write(hash, binary.LittleEndian, uint64(info.Size()))
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			n, err := io.Copy(hash, io.LimitReader(file, info.Size()+1))
			if err != nil {
				return err
			}
			if n != info.Size() {
				return fmt.Errorf("plugin file changed while fingerprinting: %s", path)
			}
		default:
			return fmt.Errorf("unsupported plugin entry type: %s", path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

func writeHashPart(w io.Writer, part string) {
	_ = binary.Write(w, binary.LittleEndian, uint64(len(part)))
	_, _ = io.WriteString(w, part)
}

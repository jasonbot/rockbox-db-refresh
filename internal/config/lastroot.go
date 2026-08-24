package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Cache of the last device root, under the user config directory
// (~/.config on Linux), so subsequent runs can default to it.
const lastRootRelPath = "rockbox-db-refresh/last.txt"

func lastRootPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, lastRootRelPath), nil
}

// ReadLastRoot returns the remembered root, or "" if none was saved.
func ReadLastRoot() string {
	p, err := lastRootPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func SaveLastRoot(path string) error {
	p, err := lastRootPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(path+"\n"), 0644)
}

package walker

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"rbdb/internal/meta"
)

func CollectDirs(ctx context.Context, root string) ([]string, error) {
	var dirs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	return dirs, err
}

func FindAudioFiles(dir string) []string {
	var files []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return files
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if meta.AudioExts[ext] {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

func RelativePath(base, path string) (string, error) {
	return filepath.Rel(base, path)
}

func OutputExists(outputPath string) bool {
	_, err := os.Stat(outputPath)
	return err == nil
}

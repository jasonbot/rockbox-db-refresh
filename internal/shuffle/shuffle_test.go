package shuffle

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"rbdb/internal/meta"
)

func TestNewPlaylistName(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 5, 0, 0, time.UTC)
	got := NewPlaylistName(now)
	want := "2026-10-14 09 New Shuffle.m3u8"
	if got != want {
		t.Fatalf("NewPlaylistName = %q, want %q", got, want)
	}
}

func TestCatalogDirDefault(t *testing.T) {
	root := t.TempDir()
	rbDir := filepath.Join(root, ".rockbox")
	if err := os.MkdirAll(rbDir, 0755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "Playlists")
	if got := CatalogDir(root, rbDir); got != want {
		t.Fatalf("CatalogDir = %q, want %q", got, want)
	}
}

func TestCatalogDirFromConfig(t *testing.T) {
	root := t.TempDir()
	rbDir := filepath.Join(root, ".rockbox")
	if err := os.MkdirAll(rbDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := "# .cfg file created by rockbox\r\nplaylist catalog directory: /MyPls\r\n"
	if err := os.WriteFile(filepath.Join(rbDir, "config.cfg"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "MyPls")
	if got := CatalogDir(root, rbDir); got != want {
		t.Fatalf("CatalogDir = %q, want %q", got, want)
	}
}

func TestWriteCatalogPlaylist(t *testing.T) {
	root := t.TempDir()
	catDir := filepath.Join(root, "Playlists")
	tracks := []*meta.Track{
		{DevPath: "/Music/b.mp3"},
		{DevPath: "/Music/a.mp3"},
	}
	now := time.Date(2026, 10, 14, 9, 5, 0, 0, time.UTC)

	name, n, err := WriteCatalogPlaylist(catDir, tracks, 0, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if name != "2026-10-14 09 New Shuffle.m3u8" {
		t.Fatalf("name = %q", name)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}

	b, err := os.ReadFile(filepath.Join(catDir, name))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	sort.Strings(got)
	want := []string{"/Music/a.mp3", "/Music/b.mp3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("playlist = %q, want %q", got, want)
	}
}

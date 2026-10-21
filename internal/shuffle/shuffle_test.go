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

func TestPickShuffled(t *testing.T) {
	tracks := []*meta.Track{
		{DevPath: "/Music/a.mp3"},
		{DevPath: "/Music/b.mp3"},
		{DevPath: "/Music/c.mp3"},
		{DevPath: "/Music/d.mp3"},
	}

	// The limit caps the pick; members are distinct tracks from the input.
	picked := PickShuffled(tracks, 2, 2.0)
	if len(picked) != 2 {
		t.Fatalf("len(picked) = %d, want 2", len(picked))
	}
	seen := map[*meta.Track]bool{}
	for _, p := range picked {
		if seen[p] {
			t.Fatalf("duplicate pick %v", p.DevPath)
		}
		seen[p] = true
	}
	for i, p := range picked {
		if p != tracks[0] && p != tracks[1] && p != tracks[2] && p != tracks[3] {
			t.Fatalf("picked[%d] = %v is not an input track", i, p.DevPath)
		}
	}

	// The input slice order is not disturbed.
	wantOrder := []string{"/Music/a.mp3", "/Music/b.mp3", "/Music/c.mp3", "/Music/d.mp3"}
	for i, tr := range tracks {
		if tr.DevPath != wantOrder[i] {
			t.Fatalf("input mutated: tracks[%d] = %q, want %q", i, tr.DevPath, wantOrder[i])
		}
	}

	// A non-positive or oversized limit means all tracks.
	for _, limit := range []int{0, -1, 99} {
		if got := PickShuffled(tracks, limit, 0); len(got) != len(tracks) {
			t.Fatalf("limit %d: len = %d, want %d", limit, len(got), len(tracks))
		}
	}
}

func albumKeyOf(t *meta.Track) string { return t.Meta.Album }

func albumOf(t *meta.Track) string { return t.Meta.Album }

func mkAlbum(album string, n int, added int64) []*meta.Track {
	var out []*meta.Track
	for i := 0; i < n; i++ {
		out = append(out, &meta.Track{
			DevPath: album + string(rune('a'+i)) + ".mp3",
			Meta:    meta.Meta{Album: album, Title: album + string(rune('a'+i))},
			Added:   added,
		})
	}
	return out
}

func TestRecencyAlbumOrderContiguous(t *testing.T) {
	var tracks []*meta.Track
	tracks = append(tracks, mkAlbum("A", 3, 100)...)
	tracks = append(tracks, mkAlbum("B", 2, 300)...)
	tracks = append(tracks, mkAlbum("C", 4, 200)...)

	for _, recency := range []float64{0, 2.0} {
		out := RecencyAlbumOrder(tracks, recency, albumKeyOf)
		if len(out) != len(tracks) {
			t.Fatalf("recency=%v: got %d tracks, want %d", recency, len(out), len(tracks))
		}
		// Albums must stay contiguous and internally ordered.
		seen := map[string]bool{}
		last := ""
		counts := map[string]int{}
		for _, tr := range out {
			a := albumOf(tr)
			if a != last {
				if seen[a] {
					t.Fatalf("recency=%v: album %s appears twice", recency, a)
				}
				seen[a] = true
				last = a
			}
			counts[a]++
		}
		for album, want := range map[string]int{"A": 3, "B": 2, "C": 4} {
			if counts[album] != want {
				t.Fatalf("recency=%v: album %s has %d tracks, want %d", recency, album, counts[album], want)
			}
		}
	}
}

func TestRecencyAlbumOrderBias(t *testing.T) {
	// Strong recency bias: the newest album should almost always lead.
	firsts := map[string]int{}
	const runs = 100
	for i := 0; i < runs; i++ {
		var tracks []*meta.Track
		tracks = append(tracks, mkAlbum("old", 2, 100)...)
		tracks = append(tracks, mkAlbum("mid", 2, 200)...)
		tracks = append(tracks, mkAlbum("new", 2, 300)...)
		out := RecencyAlbumOrder(tracks, 8.0, albumKeyOf)
		firsts[albumOf(out[0])]++
	}
	if firsts["new"] < 90 {
		t.Fatalf("newest album led %d/%d runs, expected >= 90", firsts["new"], runs)
	}
	if firsts["old"] > 5 {
		t.Fatalf("oldest album led %d/%d runs, expected <= 5", firsts["old"], runs)
	}
}

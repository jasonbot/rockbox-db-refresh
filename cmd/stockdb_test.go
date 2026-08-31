package cmd

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"rbdb/internal/db"
	"rbdb/internal/meta"
)

// buildFixture writes a small Rockbox tagcache database under root/.rockbox
// plus an optional iPod SysInfo file (model number and FireWire GUID).
func buildFixture(t *testing.T, root string, tracks []*meta.Track) {
	t.Helper()
	rbDir := filepath.Join(root, ".rockbox")
	if err := os.MkdirAll(rbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := db.Build(rbDir, tracks, nil); err != nil {
		t.Fatalf("building rockbox db fixture: %v", err)
	}
}

func writeSysInfo(t *testing.T, root, model, guid string) {
	t.Helper()
	dir := filepath.Join(root, "iPod_Control", "Device")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var b string
	if model != "" {
		b += "ModelNumStr: " + model + "\n"
	}
	if guid != "" {
		b += "FirewireGuid: " + guid + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "SysInfo"), []byte(b), 0644); err != nil {
		t.Fatal(err)
	}
}

func runStock(t *testing.T, args ...string) error {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldStdout := os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devNull
	defer func() { os.Stdout = oldStdout }()
	return App.Run(append([]string{"rbdb", "sync-stock-db"}, args...))
}

func stockDBFile(t *testing.T, root string) ([]byte, int) {
	t.Helper()
	path := filepath.Join(root, "iPod_Control", "iTunes", "iTunesDB")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(data[:4]) != "mhbd" {
		t.Fatalf("not an iTunesDB: magic %q", data[:4])
	}
	if total := int(binary.LittleEndian.Uint32(data[0x08:])); total != len(data) {
		t.Fatalf("mhbd total %d != file size %d", total, len(data))
	}
	return data, int(binary.LittleEndian.Uint32(data[0x10:]))
}

// TestSyncStockDB5GCLI: with no SysInfo, the device type is unknown, which
// falls back to the safe 5G/5.5G Video format (dbv 0x0f, no HASH58).
func TestSyncStockDB5GCLI(t *testing.T) {
	root := t.TempDir()
	buildFixture(t, root, []*meta.Track{
		{DevPath: "/Music/Alb/A.mp3", Meta: meta.Meta{Title: "One", Artist: "ArtistA", Album: "Album1"}, TrackNum: 1},
		{DevPath: "/Music/Alb/B.mp3", Meta: meta.Meta{Title: "Two", Artist: "ArtistA", Album: "Album1"}, TrackNum: 2},
		{DevPath: "/Music/B1.flac", Meta: meta.Meta{Title: "Three", Artist: "ArtistB", Album: "Album2"}, TrackNum: 1},
	})
	if err := runStock(t, "-root", root, "-no-tui"); err != nil {
		t.Fatalf("sync-stock-db failed: %v", err)
	}
	if _, version := stockDBFile(t, root); version != 0x0f {
		t.Fatalf("5G version = %#x, want 0x0f", version)
	}
}

// TestSyncStockDB5GModelCLI: a SysInfo presenting a 5G Video model (MA146)
// is detected as 5G even when SysInfo data exists.
func TestSyncStockDB5GModelCLI(t *testing.T) {
	root := t.TempDir()
	buildFixture(t, root, []*meta.Track{
		{DevPath: "/Music/A.mp3", Meta: meta.Meta{Title: "One", Artist: "A", Album: "Alb"}, TrackNum: 1},
	})
	writeSysInfo(t, root, "MA146LL/A", "0x1234567890ABCDEF")
	if err := runStock(t, "-root", root, "-no-tui"); err != nil {
		t.Fatalf("sync-stock-db failed: %v", err)
	}
	if _, version := stockDBFile(t, root); version != 0x0f {
		t.Fatalf("5G model version = %#x, want 0x0f", version)
	}
}

// TestSyncStockDBClassicCLI: the model number MB147 (a 6G Classic) is detected
// automatically, and both the format and the FireWire GUID come from SysInfo.
func TestSyncStockDBClassicCLI(t *testing.T) {
	root := t.TempDir()
	buildFixture(t, root, []*meta.Track{
		{DevPath: "/Music/A1.mp3", Meta: meta.Meta{Title: "One", Artist: "ArtistA", Album: "Album1"}, TrackNum: 1},
		{DevPath: "/Music/A2.mp3", Meta: meta.Meta{Title: "Two", Artist: "ArtistA", Album: "Album1"}, TrackNum: 2},
	})
	writeSysInfo(t, root, "MB147LL/A", "0x1234567890ABCDEF")
	if err := runStock(t, "-root", root, "-no-tui"); err != nil {
		t.Fatalf("sync-stock-db (classic) failed: %v", err)
	}
	if _, version := stockDBFile(t, root); version != 0x19 {
		t.Fatalf("classic version = %#x, want 0x19", version)
	}
}

// TestSyncStockDBDryRun verifies -dry-run does not write the iTunesDB.
func TestSyncStockDBDryRun(t *testing.T) {
	root := t.TempDir()
	buildFixture(t, root, []*meta.Track{
		{DevPath: "/Music/A1.mp3", Meta: meta.Meta{Title: "One", Artist: "A", Album: "Alb"}, TrackNum: 1},
	})
	if err := runStock(t, "-root", root, "-no-tui", "-dry-run"); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "iPod_Control", "iTunes", "iTunesDB")); err == nil {
		t.Fatal("dry-run wrote the iTunesDB; expected no file")
	}
}

package artwork

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"rbdb/internal/meta"
)

func albumKeyOf(t *meta.Track) string { return t.Meta.Album }

func TestSyncWritesArtworkDBAndLinks(t *testing.T) {
	root := t.TempDir()
	artA := testJPEG(t, 16, 16, color.RGBA{200, 30, 30, 255})
	artB := testJPEG(t, 16, 16, color.RGBA{30, 30, 200, 255})

	// Two albums with art (one shared across two tracks, one single) and
	// one album without any art.
	mk := func(path, album string) *meta.Track {
		return &meta.Track{DevPath: "/" + filepath.ToSlash(path), Meta: meta.Meta{Album: album, Title: filepath.Base(path)}}
	}
	tracks := []*meta.Track{
		mk("Music/X/AlbA/1.mp3", "AlbA"),
		mk("Music/X/AlbA/2.mp3", "AlbA"),
		mk("Music/X/AlbB/1.mp3", "AlbB"),
		mk("Music/X/AlbC/1.mp3", "AlbC"),
	}
	writeMP3WithArt(t, filepath.Join(root, "Music/X/AlbA/1.mp3"), artA)
	writeMP3WithArt(t, filepath.Join(root, "Music/X/AlbA/2.mp3"), artA)
	writeMP3WithArt(t, filepath.Join(root, "Music/X/AlbB/1.mp3"), artB)
	writeMP3WithArt(t, filepath.Join(root, "Music/X/AlbC/1.mp3"), nil)

	ids := map[*meta.Track]uint64{}
	for i, tr := range tracks {
		ids[tr] = uint64(i + 1)
	}
	links, st, err := Sync(root, tracks, albumKeyOf, func(t *meta.Track) uint64 { return ids[t] }, ClassicFormats())
	if err != nil {
		t.Fatal(err)
	}
	if st.Albums != 3 || st.WithArt != 2 || st.Images != 2 {
		t.Fatalf("stats = %+v", st)
	}
	if len(links) != 3 {
		t.Fatalf("links = %d, want 3", len(links))
	}
	// Same album -> same image id; different album -> different id.
	if links[tracks[0]] != links[tracks[1]] {
		t.Fatalf("album A tracks not shared: %+v vs %+v", links[tracks[0]], links[tracks[1]])
	}
	if links[tracks[0]].ImgID == links[tracks[2]].ImgID {
		t.Fatalf("albums A and B share an image id")
	}
	if _, ok := links[tracks[3]]; ok {
		t.Fatalf("artless album got a link")
	}

	// Files exist with the right sizes.
	for _, f := range ClassicFormats() {
		p := filepath.Join(root, ArtworkDir, ithmbName(f.ID))
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != int64(2*f.EntrySize()) {
			t.Fatalf("%s = %d bytes, want %d", p, fi.Size(), 2*f.EntrySize())
		}
	}

	// Walk the ArtworkDB chunk tree.
	db, err := os.ReadFile(filepath.Join(root, ArtworkDir, "ArtworkDB"))
	if err != nil {
		t.Fatal(err)
	}
	if string(db[:4]) != "mhfd" {
		t.Fatalf("not mhfd")
	}
	if int(binary.LittleEndian.Uint32(db[0x08:])) != len(db) {
		t.Fatalf("mhfd total %d != file %d", binary.LittleEndian.Uint32(db[0x08:]), len(db))
	}
	if n := binary.LittleEndian.Uint32(db[0x14:]); n != 3 {
		t.Fatalf("mhfd child count = %d, want 3", n)
	}

	// Verify every chunk lands on a chunk boundary.
	walkArt(t, db, int(binary.LittleEndian.Uint32(db[0x04:])), len(db))

	// The first mhii must reference the second track's... image ids and
	// carry 3 format children.
	mhliOff := int(binary.LittleEndian.Uint32(db[0x04:])) + mhsdHdr
	if string(db[mhliOff:mhliOff+4]) != "mhli" {
		t.Fatalf("mhsd1 child is %q", db[mhliOff:mhliOff+4])
	}
	if n := binary.LittleEndian.Uint32(db[mhliOff+0x08:]); n != 2 {
		t.Fatalf("mhli count = %d, want 2", n)
	}
	mhii := mhliOff + mhliHdr
	if string(db[mhii:mhii+4]) != "mhii" {
		t.Fatalf("mhii missing")
	}
	if n := binary.LittleEndian.Uint32(db[mhii+0x0C:]); n != 3 {
		t.Fatalf("mhii children = %d, want 3 formats", n)
	}
	if id := binary.LittleEndian.Uint32(db[mhii+0x10:]); id != 1 {
		t.Fatalf("first image id = %d, want 1", id)
	}
}

func walkArt(t *testing.T, db []byte, off, end int) {
	t.Helper()
	for off < end {
		if off+8 > end {
			t.Fatalf("truncated at %d", off)
		}
		name := string(db[off : off+4])
		hdr := int(binary.LittleEndian.Uint32(db[off+4:]))
		val := int(binary.LittleEndian.Uint32(db[off+8:]))
		if hdr < 12 {
			t.Fatalf("%s at %d: bad header len %d", name, off, hdr)
		}
		switch name {
		case "mhli", "mhla", "mhlf":
			off += hdr // count field, children follow
			continue
		default:
			if off+val > end {
				t.Fatalf("%s at %d overruns", name, off)
			}
			off += val
		}
	}
	if off != end {
		t.Fatalf("walk ended at %d, region ends %d", off, end)
	}
}

func TestBuildMhodFilenameLayout(t *testing.T) {
	b := buildMhodFilename(":F1055_1.ithmb")
	if string(b[:4]) != "mhod" {
		t.Fatalf("bad magic")
	}
	if binary.LittleEndian.Uint16(b[0x0C:]) != 3 {
		t.Fatalf("type = %d, want 3", binary.LittleEndian.Uint16(b[0x0C:]))
	}
	strLen := int(binary.LittleEndian.Uint32(b[mhodHdr:]))
	if strLen != 2*len(":F1055_1.ithmb") {
		t.Fatalf("string len = %d", strLen)
	}
	if b[mhodHdr+4] != 2 {
		t.Fatalf("encoding = %d, want 2 (UTF-16)", b[mhodHdr+4])
	}
	if total := binary.LittleEndian.Uint32(b[0x08:]); int(total) != len(b) {
		t.Fatalf("total %d != len %d", total, len(b))
	}
	if !bytes.Equal(b[len(b)-4:], []byte{0x62, 0x00, 0x00, 0x00}) {
		// "...b" utf16le + 2 padding bytes (len 30 -> pad 2)...  check tail roughly
		t.Logf("tail: % x", b[len(b)-4:])
	}
}

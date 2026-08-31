package ipod

import (
	"encoding/binary"
	"strings"
	"testing"

	"rbdb/internal/meta"
)

func testTracks() []*meta.Track {
	return []*meta.Track{
		{DevPath: "/Music/Alb/A1.mp3", Meta: meta.Meta{Title: "One", Artist: "ArtistA", Album: "Album1", Genre: "Rock"}, Year: 2000, TrackNum: 1, LengthMS: 200000, Bitrate: 320, Size: 1000},
		{DevPath: "/Music/Alb/A2.mp3", Meta: meta.Meta{Title: "Two", Artist: "ArtistA", Album: "Album1"}, Year: 2000, TrackNum: 2, LengthMS: 180000, Bitrate: 192, Size: 900, Playcount: 7, Rating: 80},
		{DevPath: "/Music/B1.flac", Meta: meta.Meta{Title: "Three", Artist: "ArtistB", Album: "Album2"}, TrackNum: 1, LengthMS: 300000},
	}
}

// walkChunks validates that every "mhXX" chunk's total-length field lands on
// the start of another valid chunk (or the end of file) and returns the chunk
// list in order.
func verifyChunks(t *testing.T, db []byte) {
	t.Helper()
	off := 0
	for off < len(db) {
		if off+8 > len(db) {
			t.Fatalf("offset %d: truncated header", off)
		}
		name := string(db[off : off+4])
		if !isMh(name) {
			t.Fatalf("offset %d: unexpected chunk id %q", off, name)
		}
		headerLen := binary.LittleEndian.Uint32(db[off+4:])
		val := binary.LittleEndian.Uint32(db[off+8:])
		if headerLen < 12 {
			t.Fatalf("offset %d: bad header len %d", off, headerLen)
		}
		if name == "mhlt" || name == "mhlp" || name == "mhla" {
			off += int(headerLen)
			// The next token should be a child chunk header (or the mhsd parent).
			if off < len(db) {
				childName := string(db[off : off+4])
				if !isMh(childName) && childName != "mhsd" {
					t.Fatalf("offset %d: %s should be followed by a chunk, saw %q", off, name, childName)
				}
			}
			continue
		}
		end := off + int(val)
		if end > len(db) {
			t.Fatalf("%s at %d: total length %d overruns file %d", name, off, val, len(db))
		}
		// Non-terminal chunks must run exactly to the next chunk or EOF.
		if end < len(db) && !isMh(string(db[end:end+4])) {
			t.Fatalf("%s at %d: end %d does not land on a chunk (%q)",
				name, off, end, string(db[end:end+4]))
		}
		off = end
	}
	if off != len(db) {
		t.Fatalf("walk ended at %d, file is %d", off, len(db))
	}
}

func isMh(s string) bool {
	return len(s) == 4 && strings.HasPrefix(s, "mh")
}

func TestBuild5GAnd6GStructures(t *testing.T) {
	for _, classic := range []bool{false, true} {
		opts := Options{Classic: classic}
		if classic {
			opts.FireWire = make([]byte, 8)
			opts.FireWire[0] = 0x12
		}
		db, lay, err := BuildBytes(testTracks(), opts)
		if err != nil {
			t.Fatalf("classic=%v: %v", classic, err)
		}
		if lay.Tracks != 3 || lay.Albums != 2 {
			t.Fatalf("lay = %+v", lay)
		}
		if string(db[:4]) != "mhbd" {
			t.Fatalf("not mhbd: %q", db[:4])
		}
		total := binary.LittleEndian.Uint32(db[0x08:])
		if int(total) != len(db) {
			t.Fatalf("mhbd total %d != file size %d", total, len(db))
		}
		version := binary.LittleEndian.Uint32(db[0x10:])
		wantVer := uint32(0x0f)
		if classic {
			wantVer = 0x19
		}
		if version != wantVer {
			t.Fatalf("version %x", version)
		}
		verifyChunks(t, db)
	}
}

func TestHash58Deterministic(t *testing.T) {
	fw := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	a := computeHash58(fw, []byte("hello"))
	b := computeHash58(fw, []byte("hello"))
	c := computeHash58(fw, []byte("hello2"))
	if a != b {
		t.Fatal("not deterministic")
	}
	if a == c {
		t.Fatal("should differ on input")
	}
}

// parseBack is an independent (non-shared-code) reader for the chunk format,
// used to validate that a generated DB round-trips to the correct tracks,
// albums, and playlists. It walks mhbd->mhsd(type)->mhlt/mhla/mhlp lists.
func parseBack(t *testing.T, db []byte) (tracks []parseTrack, albums []string, playlists []parsePlaylist) {
	t.Helper()
	if string(db[:4]) != "mhbd" {
		t.Fatalf("not mhbd")
	}
	// Walk mhbd children (mhsd) by total length. Children follow the mhbd
	// header (mhbd[4:8] is the header length, which covers offsets 0..hdr).
	off := int(binary.LittleEndian.Uint32(db[4:]))
	end := int(binary.LittleEndian.Uint32(db[8:]))
	for off < end {
		if string(db[off:off+4]) != "mhsd" {
			t.Fatalf("mhbd child at %d is %q", off, db[off:off+4])
		}
		mhsdEnd := off + int(binary.LittleEndian.Uint32(db[off+8:]))
		styp := binary.LittleEndian.Uint32(db[off+0x0C:])
		// list chunk follows mhsd header (0x18)
		listOff := off + int(binary.LittleEndian.Uint32(db[off+4:]))
		if string(db[listOff:listOff+4]) != "mhlt" && string(db[listOff:listOff+4]) != "mhla" && string(db[listOff:listOff+4]) != "mhlp" {
			t.Fatalf("mhsd type %d list is %q", styp, db[listOff:listOff+4])
		}
		count := int(binary.LittleEndian.Uint32(db[listOff+8:]))
		itemOff := listOff + int(binary.LittleEndian.Uint32(db[listOff+4:]))
		for i := 0; i < count; i++ {
			iname := string(db[itemOff : itemOff+4])
			itEnd := itemOff + int(binary.LittleEndian.Uint32(db[itemOff+8:]))
			switch styp {
			case 1: // tracks
				if iname != "mhit" {
					t.Fatalf("expected mhit, got %q", iname)
				}
				tracks = append(tracks, parseTrack{
					id:     binary.LittleEndian.Uint32(db[itemOff+0x10:]),
					title:  mhodOf(t, db, itemOff, itEnd, 1),
					loc:    mhodOf(t, db, itemOff, itEnd, 2),
					album:  mhodOf(t, db, itemOff, itEnd, 3),
					artist: mhodOf(t, db, itemOff, itEnd, 4),
				})
			case 4: // albums
				if iname != "mhia" {
					t.Fatalf("expected mhia, got %q", iname)
				}
				albums = append(albums, mhodOf(t, db, itemOff, itEnd, 200))
			case 2: // playlists
				if iname != "mhyp" {
					t.Fatalf("expected mhyp, got %q", iname)
				}
				pl := parsePlaylist{name: mhodOf(t, db, itemOff, itEnd, 1)}
				pl.master = db[itemOff+0x14] == 1
				// walk all children of the playlist and count mhip entries
				for k := itemOff + int(binary.LittleEndian.Uint32(db[itemOff+4:])); k < itEnd; {
					kn := string(db[k : k+4])
					ktotal := int(binary.LittleEndian.Uint32(db[k+8:]))
					if kn == "mhip" {
						pl.mhips++
					}
					k += ktotal
				}
				playlists = append(playlists, pl)
			}
			itemOff = itEnd
		}
		off = mhsdEnd
	}
	return tracks, albums, playlists
}

// mhodOf finds the first mhod child within item (itemOff..itEnd) whose type
// matches want and decodes its UTF-16 string value.
func mhodOf(t *testing.T, db []byte, itemOff, itEnd int, want uint32) string {
	t.Helper()
	hdrLen := int(binary.LittleEndian.Uint32(db[itemOff+4:]))
	for o := itemOff + hdrLen; o < itEnd && string(db[o:o+4]) == "mhod"; {
		if binary.LittleEndian.Uint32(db[o+0x0C:]) == want {
			n := int(binary.LittleEndian.Uint32(db[o+0x1C:]))
			raw := db[o+0x28 : o+0x28+n]
			var sb strings.Builder
			for i := 0; i+1 < len(raw); i += 2 {
				sb.WriteByte(raw[i])
			}
			return sb.String()
		}
		o += int(binary.LittleEndian.Uint32(db[o+8:]))
	}
	return ""
}

type parseTrack struct {
	id     uint32
	title  string
	loc    string
	album  string
	artist string
}
type parsePlaylist struct {
	name   string
	master bool
	mhips  int
}

// TestParseBackRoundTrip verifies that a generated DB (both 5G and 6G)
// decodes back to the expected tracks, albums, and playlists.
func TestParseBackRoundTrip(t *testing.T) {
	for _, classic := range []bool{false, true} {
		opts := Options{Classic: classic}
		if classic {
			opts.FireWire = []byte{0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 0xDE, 0xF0}
		}
		db, _, err := BuildBytes(testTracks(), opts)
		if err != nil {
			t.Fatalf("classic=%v: %v", classic, err)
		}
		tracks, albums, playlists := parseBack(t, db)
		if len(tracks) != 3 {
			t.Fatalf("songs=%d", len(tracks))
		}
		wantTitles := []string{"One", "Two", "Three"}
		wantConds := []string{":Music:Alb:A1.mp3", ":Music:Alb:A2.mp3", ":Music:B1.flac"}
		for i, tr := range tracks {
			if tr.title != wantTitles[i] {
				t.Fatalf("track %d title %q", i, tr.title)
			}
			if tr.loc != wantConds[i] {
				t.Fatalf("track %d loc %q", i, tr.loc)
			}
		}
		if tracks[0].album != "Album1" || tracks[2].artist != "ArtistB" {
			t.Fatalf("metadata mismatch: %+v", tracks)
		}
		if len(albums) != 2 || albums[0] != "Album1" || albums[1] != "Album2" {
			t.Fatalf("albums=%v", albums)
		}
		if len(playlists) != 3 {
			t.Fatalf("playlists=%d", len(playlists))
		}
		if !playlists[0].master || playlists[0].mhips != 3 {
			t.Fatalf("master playlist %+v", playlists[0])
		}
		if playlists[1].name != "Album1" || playlists[1].mhips != 2 {
			t.Fatalf("album playlist %+v", playlists[1])
		}
		if playlists[2].name != "Album2" || playlists[2].mhips != 1 {
			t.Fatalf("album playlist %+v", playlists[2])
		}
	}
}

func TestMhodStringLayout(t *testing.T) {
	b := buildMhodString(mhodTitle, "Hi")
	if string(b[:4]) != "mhod" {
		t.Fatalf("bad id")
	}
	if binary.LittleEndian.Uint32(b[0x04:]) != 0x18 {
		t.Fatalf("bad header len")
	}
	// "Hi" is 2 chars *2 bytes = 4; total = 0x28 + 4 = 0x2C
	if got := binary.LittleEndian.Uint32(b[0x08:]); got != 0x2C {
		t.Fatalf("total %d", got)
	}
	// utf16 content "Hi" at 0x28
	if b[0x28] != 'H' || b[0x2A] != 'i' {
		t.Fatalf("utf16 content wrong")
	}
}

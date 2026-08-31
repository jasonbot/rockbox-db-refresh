package ipod

// Builder writes an uncompressed iTunesDB for iPod Classic 5G-7G from a set
// of tracks parsed out of the Rockbox tagcache database.
//
// The iTunesDB is a tree of little-endian "mh*" chunks. This file implements
// the chunk serializers and assembles the full document. Targets:
//
//   - 5G/5.5G Video: dbversion 0x0f, mhbd header 0x68, no checksum.
//   - 6G/7G Classic: dbversion 0x19, mhbd header 0xF4, hashing_scheme 1 and a
//     HASH58 HMAC-SHA1 checksum required to boot.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rbdb/internal/meta"
)

// Options controls how the database is written.
type Options struct {
	// Classic selects the 6G/7G format (dbversion 0x19, 0xF4 header,
	// HASH58 checksum). When false the 5G/5.5G Video format (0x68 header,
	// no checksum) is used.
	Classic bool

	// FireWire is the raw 8-byte FireWire GUID used for HASH58. Required
	// when Classic is true.
	FireWire []byte
}

// Layout summarizes what was written, for reporting.
type Layout struct {
	Tracks int
	Albums int
}

const iTunesDir = "iPod_Control/iTunes"

// InstallPath returns the on-device directory of the stock database.
func InstallPath() string { return iTunesDir }

// DatabaseFile is the stock firmware database file name.
const DatabaseFile = "iTunesDB"

// mhod type codes used by this writer.
const (
	mhodTitle         = 1
	mhodLocation      = 2
	mhodAlbum         = 3
	mhodArtist        = 4
	mhodGenre         = 5
	mhodFiletype      = 6
	mhodYear          = 7
	mhodTrackNum      = 10
	mhodComposer      = 12
	mhodGrouping      = 13
	mhodDiscNum       = 16
	mhodAlbumArtist   = 22
	mhodOrder         = 100
	mhodAlbumTitle    = 200
	mhodAlbumArtistNm = 201
)

// Build writes iTunesDB under root/iPod_Control/iTunes/iTunesDB from tracks.
// Each track's DevPath is the on-device path.
func Build(root string, tracks []*meta.Track, opts Options) (*Layout, error) {
	db, lay, err := BuildBytes(tracks, opts)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, iTunesDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "iTunesDB"), db, 0644); err != nil {
		return nil, err
	}
	return lay, nil
}

// BuildBytes serializes the iTunesDB into a byte slice without touching the
// filesystem (used by -dry-run and tests).
func BuildBytes(tracks []*meta.Track, opts Options) ([]byte, *Layout, error) {
	if opts.Classic && len(opts.FireWire) < 8 {
		return nil, nil, fmt.Errorf("6G/7G format requires a FireWire GUID for HASH58 (pass -firewire-guid or provide iPod_Control/Device/SysInfo)")
	}

	tracks = filterTracks(tracks)
	albums := groupAlbums(tracks)

	// ---- MHSD 1: track list ----
	var trackChunks []byte
	for i, t := range tracks {
		trackChunks = append(trackChunks, buildMhit(t, i+1, albums.trackAlbum[i])...)
	}
	mhsd1 := wrapMhsd(1, appendChunks(chunk("mhlt", 0x14, uint32(len(tracks))), trackChunks))

	// ---- MHSD 4: album list ----
	var albumChunks []byte
	for id, rep := range albums.order {
		albumChunks = append(albumChunks, buildMhia(rep, id)...)
	}
	mhsd4 := wrapMhsd(4, appendChunks(chunk("mhla", 0x14, uint32(len(albums.order))), albumChunks))

	// ---- MHSD 2: playlists (library + one per album) ----
	var playlists []byte
	playlists = append(playlists, buildMhyp(true, "iTunes Music Library", memberIDs(tracks))...)
	for id, rep := range albums.order {
		var members []*meta.Track
		for i, t := range tracks {
			if albums.trackAlbum[i] == id {
				members = append(members, t)
			}
		}
		name := rep.Meta.Album
		if name == "" {
			name = rep.Meta.Title
		}
		playlists = append(playlists, buildMhyp(false, name, memberIDs(members))...)
	}
	mhsd2 := wrapMhsd(2, appendChunks(chunk("mhlp", 0x14, uint32(len(albums.order)+1)), playlists))

	// ---- assemble body and mhbd header ----
	headerLen := uint32(0x68)
	version := uint32(0x0f)
	if opts.Classic {
		headerLen = 0xF4
		version = 0x19
	}
	children := [][]byte{mhsd1, mhsd4, mhsd2}
	body := appendChunks(nil, children...)

	out := make([]byte, 0, int(headerLen)+len(body))
	out = append(out, make([]byte, headerLen)...)
	out = append(out, body...)
	copy(out, "mhbd")

	now := uint64(time.Now().Unix())
	u32 := binary.LittleEndian
	u32.PutUint32(out[0x04:], headerLen)
	u32.PutUint32(out[0x08:], uint32(len(out))) // mhbd total length = whole file
	u32.PutUint32(out[0x0C:], 1)
	u32.PutUint32(out[0x10:], version)
	u32.PutUint32(out[0x14:], uint32(len(children)))
	u32.PutUint64(out[0x18:], now) // db id
	u32.PutUint16(out[0x20:], 2)
	u32.PutUint64(out[0x26:], now)
	if opts.Classic {
		u32.PutUint16(out[0x30:], 1) // classic flag
	}
	u32.PutUint16(out[0x46:], 'e'|'n'<<8) // language "en"
	u32.PutUint64(out[0x48:], now)        // library persistent id

	if opts.Classic {
		if err := writeHash58(out, opts.FireWire); err != nil {
			return nil, nil, err
		}
	}

	lay := &Layout{Tracks: len(tracks), Albums: len(albums.order)}
	return out, lay, nil
}

// albumGroup maps each track to an album id and keeps unique albums in
// first-seen order.
type albumGroup struct {
	trackAlbum []int
	order      []*meta.Track
}

func groupAlbums(tracks []*meta.Track) albumGroup {
	index := map[string]int{}
	var g albumGroup
	g.trackAlbum = make([]int, len(tracks))
	for i, t := range tracks {
		key := albumKey(t)
		id, ok := index[key]
		if !ok {
			id = len(g.order)
			index[key] = id
			g.order = append(g.order, t)
		}
		g.trackAlbum[i] = id
	}
	return g
}

func albumKey(t *meta.Track) string {
	return strings.ToLower(t.Meta.Album + "\x00" + t.Meta.Artist)
}

func filterTracks(tracks []*meta.Track) []*meta.Track {
	out := tracks[:0]
	for _, t := range tracks {
		if t != nil && t.DevPath != "" {
			out = append(out, t)
		}
	}
	return out
}

func memberIDs(tracks []*meta.Track) []uint32 {
	ids := make([]uint32, len(tracks))
	for i := range tracks {
		ids[i] = uint32(i + 1)
	}
	return ids
}

// chunk writes a chunk header of the given headerLen with a value at offset 8
// (total length for most chunks, child count for mhlt/mhlp).
func chunk(name string, headerLen, value uint32) []byte {
	b := make([]byte, headerLen)
	copy(b, name)
	binary.LittleEndian.PutUint32(b[0x04:], headerLen)
	binary.LittleEndian.PutUint32(b[0x08:], value)
	return b
}

func appendChunks(prefix []byte, chunks ...[]byte) []byte {
	n := len(prefix)
	for _, c := range chunks {
		n += len(c)
	}
	out := make([]byte, 0, n)
	out = append(out, prefix...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func wrapMhsd(typ int, child []byte) []byte {
	b := chunk("mhsd", 0x18, uint32(0x18+len(child)))
	binary.LittleEndian.PutUint32(b[0x0C:], uint32(typ))
	return append(b, child...)
}

// buildMhit serializes one track item. Header is 0x184 bytes (dbv >= 0x14).
func buildMhit(t *meta.Track, id, albumID int) []byte {
	children := mhitStringChildren(t)
	total := 0x184 + len(children)

	hdr := make([]byte, 0x184)
	copy(hdr, "mhit")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], 0x184)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], uint32(mhitStringCount(t)))
	u.PutUint32(hdr[0x10:], uint32(id))     // unique id (referenced by mhip)
	u.PutUint32(hdr[0x14:], 1)              // visible
	u.PutUint32(hdr[0x18:], fileType(t))    // "MP3 " etc
	u.PutUint32(hdr[0x1C:], 0x0101)         // VBR MP3 type1/type2
	u.PutUint32(hdr[0x1F:], rating20(t))    // stars*20
	u.PutUint32(hdr[0x20:], clamp(t.MTime)) // last modified
	u.PutUint32(hdr[0x24:], clamp(t.Size))  // size bytes
	u.PutUint32(hdr[0x28:], clamp(t.LengthMS))
	u.PutUint32(hdr[0x2C:], uint32(t.TrackNum))
	u.PutUint32(hdr[0x34:], uint32(t.Year))
	u.PutUint32(hdr[0x38:], uint32(t.Bitrate))
	u.PutUint32(hdr[0x3C:], uint32(44100*0x10000)) // sample rate
	u.PutUint32(hdr[0x50:], uint32(t.Playcount))
	u.PutUint32(hdr[0x54:], uint32(t.Playcount))
	u.PutUint32(hdr[0x58:], uint32(t.LastPlayed))
	u.PutUint32(hdr[0x5C:], uint32(t.Disc))
	u.PutUint64(hdr[0x70:], uint64(id))                // dbid
	u.PutUint16(hdr[0x7C:], 0)                         // artwork count 0
	u.PutUint16(hdr[0x7E:], 0xFFFF)                    // unk9 mp3
	u.PutUint32(hdr[0x88:], math.Float32bits(44100.0)) // sample rate as float32
	u.PutUint32(hdr[0xD0:], 0x00000001)                // media type: audio
	u.PutUint16(hdr[0x13A:], uint16(albumID+1))        // AlbumID into album list

	return append(hdr, children...)
}
func mhitStringCount(t *meta.Track) int {
	n := 4 // title, artist, filetype, location
	if t.Meta.AlbumArtist != "" {
		n++
	}
	if t.Meta.Album != "" {
		n++
	}
	if t.Meta.Genre != "" {
		n++
	}
	if t.Meta.Composer != "" {
		n++
	}
	if t.Meta.Grouping != "" {
		n++
	}
	if t.Year > 0 {
		n++
	}
	if t.TrackNum > 0 {
		n++
	}
	if t.Disc > 0 {
		n++
	}
	return n
}

func mhitStringChildren(t *meta.Track) []byte {
	var b []byte
	b = append(b, buildMhodString(mhodTitle, t.Meta.Title)...)
	if t.Meta.AlbumArtist != "" {
		b = append(b, buildMhodString(mhodAlbumArtist, t.Meta.AlbumArtist)...)
	}
	if t.Meta.Album != "" {
		b = append(b, buildMhodString(mhodAlbum, t.Meta.Album)...)
	}
	b = append(b, buildMhodString(mhodArtist, firstNonEmpty(t.Meta.Artist, t.Meta.AlbumArtist))...)
	if t.Meta.Genre != "" {
		b = append(b, buildMhodString(mhodGenre, t.Meta.Genre)...)
	}
	if t.Meta.Composer != "" {
		b = append(b, buildMhodString(mhodComposer, t.Meta.Composer)...)
	}
	if t.Meta.Grouping != "" {
		b = append(b, buildMhodString(mhodGrouping, t.Meta.Grouping)...)
	}
	if t.Year > 0 {
		b = append(b, buildMhodString(mhodYear, fmt.Sprint(t.Year))...)
	}
	if t.TrackNum > 0 {
		b = append(b, buildMhodString(mhodTrackNum, fmt.Sprint(t.TrackNum))...)
	}
	if t.Disc > 0 {
		b = append(b, buildMhodString(mhodDiscNum, fmt.Sprint(t.Disc))...)
	}
	b = append(b, buildMhodString(mhodFiletype, fileTypeString(t))...)
	b = append(b, buildMhodString(mhodLocation, ipodPath(t.DevPath))...)
	return b
}

// buildMhodString serializes a UTF-16 string mhod (header length 0x18,
// string starts at offset 0x28).
func buildMhodString(typ uint32, text string) []byte {
	if text == "" {
		text = "<Untagged>"
	}
	text = strings.ToValidUTF8(text, "")
	n := utf16Len(text)
	total := 0x28 + n

	b := make([]byte, total)
	copy(b, "mhod")
	u := binary.LittleEndian
	u.PutUint32(b[0x04:], 0x18)
	u.PutUint32(b[0x08:], uint32(total))
	u.PutUint32(b[0x0C:], typ)
	u.PutUint32(b[0x18:], 1) // position
	u.PutUint32(b[0x1C:], uint32(n))
	u.PutUint32(b[0x20:], 1) // encoding flag (libgpod writes 1)
	copy(b[0x28:], utf16Bytes(text))
	return b
}

func buildMhia(rep *meta.Track, id int) []byte {
	var children []byte
	children = append(children, buildMhodString(mhodAlbumTitle, rep.Meta.Album)...)
	children = append(children, buildMhodString(mhodAlbumArtistNm, firstNonEmpty(rep.Meta.Artist, rep.Meta.AlbumArtist))...)
	total := 0x58 + len(children)

	hdr := make([]byte, 0x58)
	copy(hdr, "mhia")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], 0x58)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 2)
	u.PutUint16(hdr[0x12:], uint16(id+1)) // album id
	return append(hdr, children...)
}

// buildMhyp serializes a playlist: name mhod + one mhip per member track.
func buildMhyp(master bool, name string, memberIDs []uint32) []byte {
	var children []byte
	children = append(children, buildMhodString(mhodTitle, name)...)
	for i, id := range memberIDs {
		children = append(children, buildMhip(id, i)...)
	}
	total := 0x30 + len(children)

	hdr := make([]byte, 0x30)
	copy(hdr, "mhyp")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], 0x30)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 1) // string mhods before first mhip (the name)
	u.PutUint32(hdr[0x10:], uint32(len(memberIDs)))
	if master {
		hdr[0x14] = 1
	}
	u.PutUint32(hdr[0x18:], uint32(time.Now().Unix()))
	u.PutUint64(hdr[0x1C:], uint64(time.Now().Unix()))
	u.PutUint16(hdr[0x28:], 1) // string mhod count < 50
	u.PutUint32(hdr[0x2C:], 1) // sort order: playlist order
	return append(hdr, children...)
}

// buildMhip serializes a playlist item plus its trailing type-100 order mhod.
func buildMhip(trackID uint32, position int) []byte {
	order := buildMhodOrder(position)
	total := 0x24 + len(order)

	hdr := make([]byte, 0x24)
	copy(hdr, "mhip")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], 0x24)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 1)       // one child (the order mhod)
	u.PutUint32(hdr[0x14:], trackID) // group id
	u.PutUint32(hdr[0x18:], trackID) // track id
	u.PutUint32(hdr[0x1C:], uint32(time.Now().Unix()))
	return append(hdr, order...)
}

func buildMhodOrder(position int) []byte {
	b := make([]byte, 0x2C)
	copy(b, "mhod")
	u := binary.LittleEndian
	u.PutUint32(b[0x04:], 0x18)
	u.PutUint32(b[0x08:], 0x2C)
	u.PutUint32(b[0x0C:], mhodOrder)
	u.PutUint32(b[0x18:], uint32(position))
	return b
}

// helpers ----------------------------------------------------------------

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func rating20(t *meta.Track) uint32 {
	r := t.Rating
	if r <= 0 {
		return 0
	}
	if r > 100 {
		r = 100
	}
	return uint32(r * 5) // stars * 20
}

func clamp(v int64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 0xffffffff {
		return 0xffffffff
	}
	return uint32(v)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n * 2
}

func utf16Bytes(s string) []byte {
	var buf bytes.Buffer
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			buf.WriteByte(byte(0xD800 + (r >> 10)))
			buf.WriteByte(byte((0xD800 + (r >> 10)) >> 8))
			buf.WriteByte(byte(0xDC00 + (r & 0x3FF)))
			buf.WriteByte(byte((0xDC00 + (r & 0x3FF)) >> 8))
		} else {
			buf.WriteByte(byte(r))
			buf.WriteByte(byte(r >> 8))
		}
	}
	return buf.Bytes()
}

// fileType returns the 4-byte ANSI file-type token ("MP3 ", "M4A ", etc).
func fileType(t *meta.Track) uint32 {
	s := fileTypeString(t)
	var u uint32
	for i := 0; i < 4 && i < len(s); i++ {
		u |= uint32(s[i]) << (8 * i)
	}
	return u
}

func fileTypeString(t *meta.Track) string {
	trimmed := strings.TrimSuffix(t.DevPath, " ")
	ext := strings.ToLower(pathExt(trimmed))
	switch ext {
	case ".mp3", ".mp2":
		return "MP3 "
	case ".m4a":
		return "M4A "
	case ".m4b":
		return "M4B "
	case ".flac":
		return "FLAC"
	case ".ogg", ".oga":
		return "OGG "
	case ".wav", ".wv":
		return "WAV "
	default:
		return "    "
	}
}

func pathExt(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[i:]
	}
	return ""
}

// ipodPath converts a device path ("/Music/A.mp3") into the iTunesDB location
// token (":Music:A.mp3").
func ipodPath(devPath string) string {
	if devPath == "" {
		return ":"
	}
	p := strings.TrimPrefix(devPath, "/")
	return ":" + strings.ReplaceAll(p, "/", ":")
}

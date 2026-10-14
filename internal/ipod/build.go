package ipod

// Builder writes an uncompressed iTunesDB for iPod Classic 5G-7G from a set
// of tracks parsed out of the Rockbox tagcache database.
//
// The iTunesDB is a tree of little-endian "mh*" chunks. This file implements
// the chunk serializers and assembles the full document. Two on-disk layouts
// are supported:
//
//   - 5G/5.5G Video: dbversion 0x0f, compact headers (mhbd 0x68, mhit 0x184),
//     no checksum. This is the historical Apple 5G layout.
//   - 6G/7G Classic: dbversion 0x30, the full modern layout that libgpod
//     emits (mhbd 0xF4, mhsd 0x60, mhlt/mhlp/mhla 0x5C, mhit 0x248, mhyp
//     0x6C, mhip 0x4C, mhia 0x58), hashing_scheme 1 and a HASH58 HMAC-SHA1
//     checksum required to boot. The firmware rejects a database whose
//     library version is older than the device firmware, so the current
//     (iTunes 9.2) version 0x30 is used rather than the 6G-era 0x19.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"rbdb/internal/artwork"
	"rbdb/internal/meta"
)

// Options controls how the database is written.
type Options struct {
	// Classic selects the 6G/7G format (dbversion 0x30, 0xF4 header,
	// HASH58 checksum). When false the 5G/5.5G Video format (0x68 header,
	// no checksum) is used.
	Classic bool

	// FireWire is the raw 8-byte FireWire GUID used for HASH58. Required
	// when Classic is true.
	FireWire []byte

	// Artwork links each track (by pointer) to its image in the ArtworkDB.
	// Linked tracks get artwork_count/artwork_size/has_artwork/mhii_link set
	// in their mhit header; the fields live in the fixed-size header, so
	// this never changes the database size. Nil means no artwork.
	Artwork map[*meta.Track]artwork.ArtLink
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
	mhodLibraryIndex  = 52
	mhodJumpTable     = 53
	mhodAlbumTitle    = 200
	mhodAlbumArtistNm = 201
)

// mhod52 sort types (the value stored inside a type-52 library index mhod).
const (
	sortTitle    = 0x03
	sortAlbum    = 0x04
	sortArtist   = 0x05
	sortGenre    = 0x07
	sortComposer = 0x12
)

// dbFormat describes the on-disk chunk layout variant.
type dbFormat struct {
	classic bool
	version uint32
	mhbdHdr uint32
	mhsdHdr uint32
	mhltHdr uint32
	mhitHdr uint32
	mhypHdr uint32
	mhipHdr uint32
	mhiaHdr uint32
	// datasets is the ordered set of mhsd dataset types to emit. The
	// Classic firmware expects the full Apple set (tracks, podcast
	// playlists, playlists, albums, artists, and the empty helper
	// datasets 6/10/5); the 5G path keeps the historical three.
	datasets []int
}

func formatFor(classic bool) dbFormat {
	if classic {
		return dbFormat{
			classic:  true,
			version:  0x30,
			mhbdHdr:  0xF4,
			mhsdHdr:  0x60,
			mhltHdr:  0x5C,
			mhitHdr:  0x248,
			mhypHdr:  0x6C,
			mhipHdr:  0x4C,
			mhiaHdr:  0x58,
			datasets: []int{1, 3, 2, 4, 8, 6, 10, 5},
		}
	}
	return dbFormat{
		classic:  false,
		version:  0x0f,
		mhbdHdr:  0x68,
		mhsdHdr:  0x18,
		mhltHdr:  0x14,
		mhitHdr:  0x184,
		mhypHdr:  0x30,
		mhipHdr:  0x24,
		mhiaHdr:  0x58,
		datasets: []int{1, 4, 2},
	}
}

// Build writes iTunesDB under root/iPod_Control/iTunes/iTunesDB from tracks.
// Each track's DevPath is the on-device path.
func Build(root string, tracks []*meta.Track, opts Options) (*Layout, error) {
	db, lay, err := BuildBytes(tracks, opts)
	if err != nil {
		return nil, err
	}
	if err := WriteDatabase(root, db); err != nil {
		return nil, err
	}
	return lay, nil
}

// WriteDatabase writes a serialized iTunesDB to
// root/iPod_Control/iTunes/iTunesDB.
func WriteDatabase(root string, db []byte) error {
	dir := filepath.Join(root, iTunesDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, DatabaseFile), db, 0644)
}

// FitBytes builds the database like BuildBytes, but if the result exceeds
// maxBytes it rebuilds from a prefix of tracks, which must be ordered
// most-preferred first. It returns the data, the layout, and how many
// trailing tracks were dropped. The prefix length is estimated from the
// measured bytes-per-track and refined over a few rebuilds.
func FitBytes(tracks []*meta.Track, opts Options, maxBytes int) ([]byte, *Layout, int, error) {
	data, lay, err := BuildBytes(tracks, opts)
	if err != nil || len(data) <= maxBytes {
		return data, lay, 0, err
	}
	n := len(tracks) * maxBytes / len(data)
	for i := 0; i < 8; i++ {
		if n >= len(tracks) {
			n = len(tracks) - 1
		}
		if n < 1 {
			n = 1
		}
		data, lay, err = BuildBytes(tracks[:n], opts)
		if err != nil {
			return nil, nil, 0, err
		}
		if len(data) <= maxBytes {
			return data, lay, len(tracks) - n, nil
		}
		next := int(float64(n) * float64(maxBytes) / float64(len(data)) * 0.98)
		if next >= n {
			next = n - 1
		}
		n = next
	}
	return nil, nil, 0, fmt.Errorf("iTunesDB does not fit in %d bytes", maxBytes)
}

// BuildBytes serializes the iTunesDB into a byte slice without touching the
// filesystem (used by -dry-run and tests).
func BuildBytes(tracks []*meta.Track, opts Options) ([]byte, *Layout, error) {
	if opts.Classic && len(opts.FireWire) < 8 {
		return nil, nil, fmt.Errorf("6G/7G format requires a FireWire GUID for HASH58 (pass -firewire-guid or provide iPod_Control/Device/SysInfo)")
	}

	f := formatFor(opts.Classic)
	tracks = filterTracks(tracks)
	albums := groupAlbums(tracks)

	now := uint64(time.Now().Unix())

	// Assign a stable 1-based id to each track and build the track dataset.
	ids := make([]uint32, len(tracks))
	var trackChunks []byte
	for i, t := range tracks {
		id := uint32(i + 1)
		ids[i] = id
		art, hasArt := opts.Artwork[t]
		trackChunks = append(trackChunks, buildMhit(f, t, id, albums.trackAlbum[i], now, art, hasArt)...)
	}

	ds := map[int][]byte{}
	ds[1] = wrapMhsd(f, 1, appendChunks(chunk("mhlt", f.mhltHdr, uint32(len(tracks))), trackChunks))

	// Album list (mhsd type 4).
	var albumChunks []byte
	for id, rep := range albums.order {
		albumChunks = append(albumChunks, buildMhia(f, rep, id)...)
	}
	ds[4] = wrapMhsd(f, 4, appendChunks(chunk("mhla", f.mhltHdr, uint32(len(albums.order))), albumChunks))

	// Playlist list (mhsd type 2): only the master library playlist, matching
	// Apple/libgpod. Album browsing uses the type-4 dataset above; per-album
	// playlists are intentionally not emitted.
	playlists := buildMhyp(f, true, "iPod", tracks, ids, now, now)
	ds[2] = wrapMhsd(f, 2, appendChunks(chunk("mhlp", f.mhltHdr, 1), playlists))

	// Dataset type 3 (the podcast-playlist dataset) mirrors the regular
	// playlist list exactly as libgpod does: the Classic firmware reads the
	// master playlist's browse indexes from there, so an empty stub boots
	// but leaves the Songs/Albums/Artists menus dead.
	if f.classic {
		ds[3] = wrapMhsd(f, 3, appendChunks(chunk("mhlp", f.mhltHdr, 1), playlists))
		ds[8] = wrapMhsd(f, 8, chunk("mhli", f.mhltHdr, 0))
		ds[6] = wrapMhsd(f, 6, chunk("mhlt", f.mhltHdr, 0))
		ds[10] = wrapMhsd(f, 10, chunk("mhlt", f.mhltHdr, 0))
		ds[5] = wrapMhsd(f, 5, chunk("mhlp", f.mhltHdr, 0))
	}

	var children [][]byte
	for _, typ := range f.datasets {
		children = append(children, ds[typ])
	}
	body := appendChunks(nil, children...)

	out := make([]byte, 0, int(f.mhbdHdr)+len(body))
	out = append(out, make([]byte, f.mhbdHdr)...)
	out = append(out, body...)
	copy(out, "mhbd")

	u32 := binary.LittleEndian
	u32.PutUint32(out[0x04:], f.mhbdHdr)
	u32.PutUint32(out[0x08:], uint32(len(out))) // mhbd total length = whole file
	u32.PutUint32(out[0x0C:], 1)
	u32.PutUint32(out[0x10:], f.version)
	u32.PutUint32(out[0x14:], uint32(len(children)))
	u32.PutUint64(out[0x18:], now) // db id
	u32.PutUint16(out[0x20:], 2)
	u32.PutUint64(out[0x24:], now)        // unknown id, mirrored into each mhit
	u32.PutUint16(out[0x46:], 'e'|'n'<<8) // language "en"
	u32.PutUint64(out[0x48:], now)        // library persistent id
	if f.classic {
		u32.PutUint16(out[0x30:], 1) // hashing_scheme (rewritten by writeHash58)
	}

	if f.classic {
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

// AlbumKey groups tracks into albums exactly the way the builder does, so a
// subsetter can drop whole albums without splitting any of them.
func AlbumKey(t *meta.Track) string { return albumKey(t) }

func filterTracks(tracks []*meta.Track) []*meta.Track {
	out := tracks[:0]
	for _, t := range tracks {
		if t != nil && t.DevPath != "" {
			out = append(out, t)
		}
	}
	return out
}

// chunk writes a chunk header of the given headerLen with a value at offset 8
// (total length for most chunks, child count for mhlt/mhlp/mhla).
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

func wrapMhsd(f dbFormat, typ int, child []byte) []byte {
	b := chunk("mhsd", f.mhsdHdr, uint32(int(f.mhsdHdr)+len(child)))
	binary.LittleEndian.PutUint32(b[0x0C:], uint32(typ))
	return append(b, child...)
}

// buildMhit serializes one track item, selecting the layout variant.
func buildMhit(f dbFormat, t *meta.Track, id uint32, albumID int, libID uint64, art artwork.ArtLink, hasArt bool) []byte {
	children := mhitStringChildren(t)
	if f.classic {
		return buildMhitClassic(f, t, id, albumID, libID, children, art, hasArt)
	}
	return buildMhitLegacy(f, t, id, albumID, children)
}

// buildMhitLegacy writes the compact 0x184-byte 5G track header.
func buildMhitLegacy(f dbFormat, t *meta.Track, id uint32, albumID int, children []byte) []byte {
	total := int(f.mhitHdr) + len(children)

	hdr := make([]byte, f.mhitHdr)
	copy(hdr, "mhit")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhitHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], uint32(mhitStringCount(t)))
	u.PutUint32(hdr[0x10:], id)             // unique id (referenced by mhip)
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

// buildMhitClassic writes the full 0x248-byte 6G/7G track header that libgpod
// emits, including the album id at 0x120 and the 0x808080808080 sort flags.
// hasArt wires the artwork fields (artwork count/size at 0x7C/0x80,
// has_artwork at 0xA4, mhii_link at 0x160) that the firmware follows into the
// ArtworkDB for Cover Flow and the Now Playing screen.
func buildMhitClassic(f dbFormat, t *meta.Track, id uint32, albumID int, libID uint64, children []byte, art artwork.ArtLink, hasArt bool) []byte {
	total := int(f.mhitHdr) + len(children)

	hdr := make([]byte, f.mhitHdr)
	copy(hdr, "mhit")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhitHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], uint32(mhitStringCount(t)))
	u.PutUint32(hdr[0x10:], id)             // track id
	u.PutUint32(hdr[0x14:], 1)              // visible
	u.PutUint32(hdr[0x18:], fileType(t))    // "MP3 " etc
	hdr[0x1C] = 1                           // type1
	hdr[0x1D] = 1                           // type2
	hdr[0x1F] = byte(rating20(t))           // rating (single byte)
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
	u.PutUint64(hdr[0x70:], uint64(id)) // dbid
	if hasArt {
		u.PutUint16(hdr[0x7C:], 1)           // artwork count
		u.PutUint32(hdr[0x80:], art.SrcSize) // source image bytes
		hdr[0xA4] = 1                        // has artwork
		u.PutUint32(hdr[0x160:], art.ImgID)  // mhii_link -> ArtworkDB
	} else {
		u.PutUint16(hdr[0x7C:], 0) // artwork count 0
	}
	u.PutUint16(hdr[0x7E:], 0xFFFF)                    // unk9 mp3
	u.PutUint32(hdr[0x88:], math.Float32bits(44100.0)) // sample rate as float32
	u.PutUint32(hdr[0xD0:], 0x00000001)                // media type: audio
	u.PutUint32(hdr[0x120:], uint32(albumID+1))        // album id
	u.PutUint64(hdr[0x124:], libID)                    // same as mhbd+0x24
	u.PutUint32(hdr[0x12C:], clamp(t.Size))            // size again
	u.PutUint64(hdr[0x134:], 0x0000808080808080)       // sort flags
	u.PutUint32(hdr[0x168:], 1)                        // unknown, always 1

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
	if t.Meta.Grouping != "" && t.Meta.Grouping != t.Meta.Title {
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
	// Grouping is skipped when it merely duplicates the title (the Rockbox
	// tagcache fills it that way), which is pure dead weight on the device.
	if t.Meta.Grouping != "" && t.Meta.Grouping != t.Meta.Title {
		b = append(b, buildMhodString(mhodGrouping, t.Meta.Grouping)...)
	}
	// Year/track/disc numbers live in the mhit header already; libgpod and
	// iTunes do not duplicate them as string mhods, so neither do we.
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
	u.PutUint32(b[0x18:], 1) // string type UTF-16
	u.PutUint32(b[0x1C:], uint32(n))
	u.PutUint32(b[0x20:], 1) // unknown, always 1
	copy(b[0x28:], utf16Bytes(text))
	return b
}

func buildMhia(f dbFormat, rep *meta.Track, id int) []byte {
	var children []byte
	children = append(children, buildMhodString(mhodAlbumTitle, rep.Meta.Album)...)
	children = append(children, buildMhodString(mhodAlbumArtistNm, firstNonEmpty(rep.Meta.Artist, rep.Meta.AlbumArtist))...)
	total := int(f.mhiaHdr) + len(children)

	hdr := make([]byte, f.mhiaHdr)
	copy(hdr, "mhia")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhiaHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 2)
	if f.classic {
		u.PutUint32(hdr[0x10:], uint32(id+1)) // album id
		u.PutUint32(hdr[0x1C:], 2)            // content type flag
	} else {
		u.PutUint16(hdr[0x12:], uint16(id+1)) // legacy album id
	}
	return append(hdr, children...)
}

// buildMhyp serializes a playlist: name mhod + one mhip per member track.
func buildMhyp(f dbFormat, master bool, name string, tracks []*meta.Track, memberIDs []uint32, plID, now uint64) []byte {
	if f.classic {
		return buildMhypClassic(f, master, name, tracks, memberIDs, plID, now)
	}
	return buildMhypLegacy(f, master, name, memberIDs, now)
}

func buildMhypLegacy(f dbFormat, master bool, name string, memberIDs []uint32, now uint64) []byte {
	var children []byte
	children = append(children, buildMhodString(mhodTitle, name)...)
	for i, id := range memberIDs {
		children = append(children, buildMhipLegacy(f, id, i)...)
	}
	total := int(f.mhypHdr) + len(children)

	hdr := make([]byte, f.mhypHdr)
	copy(hdr, "mhyp")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhypHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 1) // string mhods before first mhip (the name)
	u.PutUint32(hdr[0x10:], uint32(len(memberIDs)))
	if master {
		hdr[0x14] = 1
	}
	u.PutUint32(hdr[0x18:], uint32(now))
	u.PutUint64(hdr[0x1C:], now)
	u.PutUint16(hdr[0x28:], 1) // string mhod count < 50
	u.PutUint32(hdr[0x2C:], 1) // sort order: playlist order
	return append(hdr, children...)
}

func buildMhypClassic(f dbFormat, master bool, name string, tracks []*meta.Track, memberIDs []uint32, plID, now uint64) []byte {
	var children []byte
	children = append(children, buildMhodString(mhodTitle, name)...)
	children = append(children, buildLongMhodOrder()...)

	// A non-empty master playlist carries ten more child mhods: five type-52
	// library index lists and five type-53 letter jump tables (title, artist,
	// album, genre, composer). libgpod emits these for every master playlist
	// that has members, and the Classic firmware expects them.
	mhodCount := 2
	if master && len(memberIDs) > 0 {
		children = append(children, buildPlaylistIndexes(tracks)...)
		mhodCount += 10
	}

	for i, id := range memberIDs {
		children = append(children, buildMhipClassic(f, id, i, now)...)
	}
	total := int(f.mhypHdr) + len(children)

	hdr := make([]byte, f.mhypHdr)
	copy(hdr, "mhyp")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhypHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], uint32(mhodCount))
	u.PutUint32(hdr[0x10:], uint32(len(memberIDs)))
	if master {
		hdr[0x14] = 1
	}
	u.PutUint32(hdr[0x18:], uint32(now))
	u.PutUint64(hdr[0x1C:], plID)
	u.PutUint16(hdr[0x28:], 1) // string mhod count
	u.PutUint32(hdr[0x2C:], 1) // sort order: playlist order
	return append(hdr, children...)
}

// buildPlaylistIndexes emits the five type-52 library indexes and the five
// matching type-53 letter jump tables for a master playlist, in the order
// libgpod writes them (title, artist, album, genre, composer).
func buildPlaylistIndexes(tracks []*meta.Track) []byte {
	n := len(tracks)
	keys := make([]sortKeyTrack, n)
	for i, t := range tracks {
		keys[i] = makeSortKeyTrack(t)
	}

	sortTypes := []uint32{sortTitle, sortArtist, sortAlbum, sortGenre, sortComposer}
	var out []byte
	for _, st := range sortTypes {
		order := sortedPlaylistOrder(keys, st)
		out = append(out, buildMhod52(st, order)...)
		out = append(out, buildMhod53(st, keys, order)...)
	}
	return out
}

// sortKeyTrack holds the per-track values used to order a master playlist.
type sortKeyTrack struct {
	title, album, artist, genre, composer string
	cd, trackNr                           int
}

func makeSortKeyTrack(t *meta.Track) sortKeyTrack {
	return sortKeyTrack{
		title:    t.Meta.Title,
		album:    t.Meta.Album,
		artist:   sortArtistName(t),
		genre:    t.Meta.Genre,
		composer: t.Meta.Composer,
		cd:       t.Disc,
		trackNr:  t.TrackNum,
	}
}

// sortArtistName mirrors libgpod's get_sort_artist(): an artist of the form
// "The X" is keyed as "X, The".
func sortArtistName(t *meta.Track) string {
	a := firstNonEmpty(t.Meta.Artist, t.Meta.AlbumArtist)
	if len(a) >= 4 && strings.EqualFold(a[:4], "The ") {
		return a[4:] + ", The"
	}
	return a
}

func sortedPlaylistOrder(keys []sortKeyTrack, sortType uint32) []int {
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	less := func(a, b int) bool {
		ka, kb := keys[a], keys[b]
		switch sortType {
		case sortTitle:
			return ka.title < kb.title
		case sortAlbum:
			return albumLess(ka, kb)
		case sortArtist:
			return artistLess(ka, kb)
		case sortGenre:
			return genreLess(ka, kb)
		case sortComposer:
			return composerLess(ka, kb)
		}
		return false
	}
	sort.SliceStable(order, func(i, j int) bool { return less(order[i], order[j]) })
	return order
}

func albumLess(a, b sortKeyTrack) bool {
	if a.album != b.album {
		return a.album < b.album
	}
	return trackOrderLess(a, b)
}

func artistLess(a, b sortKeyTrack) bool {
	if a.artist != b.artist {
		return a.artist < b.artist
	}
	if a.album != b.album {
		return a.album < b.album
	}
	return trackOrderLess(a, b)
}

func genreLess(a, b sortKeyTrack) bool {
	if a.genre != b.genre {
		return a.genre < b.genre
	}
	return artistLess(a, b)
}

func composerLess(a, b sortKeyTrack) bool {
	if a.composer != b.composer {
		return a.composer < b.composer
	}
	return albumLess(a, b)
}

func trackOrderLess(a, b sortKeyTrack) bool {
	if a.cd != b.cd {
		return a.cd < b.cd
	}
	if a.trackNr != b.trackNr {
		return a.trackNr < b.trackNr
	}
	return a.title < b.title
}

// buildMhod52 writes a type-52 library index: a permutation of playlist
// positions ordered by the given sort type.
func buildMhod52(sortType uint32, order []int) []byte {
	total := 72 + 4*len(order)
	b := make([]byte, total)
	copy(b, "mhod")
	u := binary.LittleEndian
	u.PutUint32(b[0x04:], 0x18)
	u.PutUint32(b[0x08:], uint32(total))
	u.PutUint32(b[0x0C:], mhodLibraryIndex)
	u.PutUint32(b[0x18:], sortType)
	u.PutUint32(b[0x1C:], uint32(len(order)))
	off := 0x48
	for _, idx := range order {
		u.PutUint32(b[off:], uint32(idx))
		off += 4
	}
	return b
}

// buildMhod53 writes a type-53 letter jump table derived from a sorted order.
func buildMhod53(sortType uint32, keys []sortKeyTrack, order []int) []byte {
	type entry struct {
		letter rune
		start  uint32
		count  uint32
	}
	var entries []entry
	for i, idx := range order {
		l := indexLetter(keys[idx], sortType)
		if len(entries) == 0 || entries[len(entries)-1].letter != l {
			entries = append(entries, entry{letter: l, start: uint32(i)})
		}
		entries[len(entries)-1].count++
	}

	total := 40 + 12*len(entries)
	b := make([]byte, total)
	copy(b, "mhod")
	u := binary.LittleEndian
	u.PutUint32(b[0x04:], 0x18)
	u.PutUint32(b[0x08:], uint32(total))
	u.PutUint32(b[0x0C:], mhodJumpTable)
	u.PutUint32(b[0x18:], sortType)
	u.PutUint32(b[0x1C:], uint32(len(entries)))
	off := 0x28
	for _, e := range entries {
		u.PutUint16(b[off:], uint16(e.letter))
		u.PutUint32(b[off+4:], e.start)
		u.PutUint32(b[off+8:], e.count)
		off += 12
	}
	return b
}

// indexLetter returns the fast-scroll index letter for a track's value under a
// sort type: the first letter uppercased, or '0' for digits/non-letters.
func indexLetter(k sortKeyTrack, sortType uint32) rune {
	var s string
	switch sortType {
	case sortTitle:
		s = k.title
	case sortAlbum:
		s = k.album
	case sortArtist:
		s = k.artist
	case sortGenre:
		s = k.genre
	case sortComposer:
		s = k.composer
	}
	for _, r := range s {
		if unicode.IsLetter(r) {
			return unicode.ToUpper(r)
		}
		if unicode.IsDigit(r) {
			return '0'
		}
	}
	return '0'
}

// buildMhip serializes a playlist item plus its trailing type-100 order mhod.
func buildMhip(f dbFormat, trackID uint32, position int, now uint64) []byte {
	if f.classic {
		return buildMhipClassic(f, trackID, position, now)
	}
	return buildMhipLegacy(f, trackID, position)
}

func buildMhipLegacy(f dbFormat, trackID uint32, position int) []byte {
	order := buildMhodOrder(position)
	total := int(f.mhipHdr) + len(order)

	hdr := make([]byte, f.mhipHdr)
	copy(hdr, "mhip")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhipHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 1)       // one child (the order mhod)
	u.PutUint32(hdr[0x14:], trackID) // group id
	u.PutUint32(hdr[0x18:], trackID) // track id
	u.PutUint32(hdr[0x1C:], uint32(time.Now().Unix()))
	return append(hdr, order...)
}

func buildMhipClassic(f dbFormat, trackID uint32, position int, now uint64) []byte {
	order := buildMhodOrder(position)
	total := int(f.mhipHdr) + len(order)

	hdr := make([]byte, f.mhipHdr)
	copy(hdr, "mhip")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], f.mhipHdr)
	u.PutUint32(hdr[0x08:], uint32(total))
	u.PutUint32(hdr[0x0C:], 1)           // one child (the order mhod)
	u.PutUint32(hdr[0x10:], 0)           // podcast group flag
	u.PutUint32(hdr[0x14:], 0)           // podcast group id
	u.PutUint32(hdr[0x18:], trackID)     // track id
	u.PutUint32(hdr[0x1C:], uint32(now)) // timestamp
	u.PutUint32(hdr[0x20:], 0)           // podcast group ref
	return append(hdr, order...)
}

// buildMhodOrder writes the short type-100 playlist position mhod placed
// inside an mhip.
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

// longMhodOrder is the fixed 0x288-byte type-100 "playlist preferences" mhod
// that libgpod attaches to every playlist. Ported verbatim from
// mk_long_mhod_id_playlist().
var longMhodOrder = func() []byte {
	b := make([]byte, 0x288)
	copy(b, "mhod")
	u := binary.LittleEndian
	u.PutUint32(b[0x04:], 0x18)
	u.PutUint32(b[0x08:], 0x288)
	u.PutUint32(b[0x0C:], mhodOrder)
	u.PutUint32(b[0x28:], 0x010084)
	u.PutUint32(b[0x2C:], 0x05)
	u.PutUint32(b[0x30:], 0x09)
	u.PutUint32(b[0x34:], 0x03)
	u.PutUint32(b[0x38:], 0x120001)
	u.PutUint32(b[0x48:], 0xc80002)
	u.PutUint32(b[0x58:], 0x3c000d)
	u.PutUint32(b[0x68:], 0x7d0004)
	u.PutUint32(b[0x78:], 0x7d0003)
	u.PutUint32(b[0x88:], 0x640008)
	u.PutUint32(b[0x98:], 0x640017)
	u.PutUint32(b[0x9C:], 0x01)
	u.PutUint32(b[0xA8:], 0x500014)
	u.PutUint32(b[0xAC:], 0x01)
	u.PutUint32(b[0xB8:], 0x7d0015)
	u.PutUint32(b[0xBC:], 0x01)
	return b
}()

func buildLongMhodOrder() []byte {
	out := make([]byte, len(longMhodOrder))
	copy(out, longMhodOrder)
	return out
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

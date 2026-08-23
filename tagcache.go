package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	tcMagic  = 0x54434810
	tagCount = 23
)

// Must match enum tag_type in apps/tagcache.h exactly.
const (
	tagArtist = iota // 0
	tagAlbum
	tagGenre
	tagTitle
	tagFilename
	tagComposer
	tagComment
	tagAlbumArtist
	tagGrouping
	tagYear
	tagDiscNumber
	tagTrackNumber
	tagCanonicalArtist
	tagBitrate
	tagLength
	tagPlaycount
	tagRating
	tagPlaytime
	tagLastPlayed
	tagCommitID
	tagMtime
	tagLastElapsed
	tagLastOffset
)

var stringTags = []int{tagArtist, tagAlbum, tagGenre, tagTitle, tagFilename,
	tagComposer, tagComment, tagAlbumArtist, tagGrouping, tagCanonicalArtist}

func isNumericTag(t int) bool {
	switch t {
	case tagYear, tagDiscNumber, tagTrackNumber, tagBitrate, tagLength,
		tagPlaycount, tagRating, tagPlaytime, tagLastPlayed, tagCommitID,
		tagMtime, tagLastElapsed, tagLastOffset:
		return true
	}
	return false
}

const untagged = "<Untagged>"
const chunkLen = 8

// maxI32 is the hard ceiling for every on-disk field (the format uses
// int32/uint32 everywhere). Offsets, datasizes and record lengths are
// checked against it so an oversized library fails loudly instead of
// silently wrapping and producing a corrupt database.
const maxI32 = int64(math.MaxInt32)

// maxStringTagLen caps non-path string tags (rune-safe). Paths are capped
// separately and much higher: the device must be able to match them
// verbatim against the filesystem during incremental updates.
var maxStringTagLen = 512

const maxPathTagLen = 1024

type indexEntry struct {
	seek [tagCount]int32
	flag int32
}

func putU32le(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
}

func tagValue(t *Track, tag int) string {
	if tag == tagFilename {
		return t.DevPath
	}
	var s string
	switch tag {
	case tagTitle:
		s = t.Meta.Title
	case tagArtist:
		s = t.Meta.Artist
	case tagAlbum:
		s = t.Meta.Album
	case tagGenre:
		s = t.Meta.Genre
	case tagComposer:
		s = t.Meta.Composer
	case tagComment:
		s = t.Meta.Comment
	case tagAlbumArtist:
		s = t.Meta.AlbumArtist
	case tagGrouping:
		if t.Meta.Grouping != "" {
			s = t.Meta.Grouping
		} else {
			s = t.Meta.Title
		}
	case tagCanonicalArtist:
		if t.Meta.Artist != "" {
			s = t.Meta.Artist
		} else {
			s = t.Meta.AlbumArtist
		}
	default:
		return untagged
	}
	return nonEmpty(sanitize(s))
}

func tagNumeric(t *Track, tag int) int32 {
	switch tag {
	case tagYear:
		return clampI32(int64(t.Year))
	case tagDiscNumber:
		return clampI32(int64(t.Disc))
	case tagTrackNumber:
		return clampI32(int64(t.TrackNum))
	case tagLength:
		return clampI32(t.LengthMS)
	case tagBitrate:
		return clampI32(int64(t.Bitrate))
	case tagMtime:
		return clampI32(t.MTime)
	case tagPlaycount:
		return clampI32(t.Playcount)
	case tagRating:
		return clampI32(t.Rating)
	case tagPlaytime:
		return clampI32(t.Playtime)
	case tagLastPlayed:
		return clampI32(t.LastPlayed)
	case tagLastElapsed:
		return clampI32(t.LastElapsed)
	case tagLastOffset:
		return clampI32(t.LastOffset)
	}
	return 0
}

func clampI32(v int64) int32 {
	if v > 0x7fffffff {
		return 0x7fffffff
	}
	if v < -0x80000000 {
		return -0x80000000
	}
	return int32(v)
}

func nonEmpty(s string) string {
	if s == "" {
		return untagged
	}
	return s
}

// sanitize enforces C-string semantics on metadata: the firmware stores
// NUL-terminated strings, so anything from the first NUL onward is lost
// anyway — drop it up front (along with control characters) so dedup,
// sorting and record sizes stay consistent.
func sanitize(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func foldLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// compare mirrors tagcache.c compare(): <Untagged> first, then ASCII
// case-insensitive strcmp order.
func tagCompare(a, b string) int {
	if a == untagged || b == untagged {
		if a == untagged && b == untagged {
			return 0
		}
		if a == untagged {
			return -1
		}
		return 1
	}
	la, lb := foldLower(a), foldLower(b)
	if la < lb {
		return -1
	}
	if la > lb {
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type uniqEntry struct {
	str    string
	offset int32
}

// writeTagFile writes one database_<tag>.tcd and returns per-track seek
// offsets. unique: deduplicate case-insensitively (idx_id=-1).
// sorted: case-insensitive sort, <Untagged> first, records padded to 8 bytes.
func writeTagFile(dir string, tag int, tracks []*Track, unique bool) ([]int32, error) {
	type rec struct {
		str    string
		idxIDs []int // master index ids referencing this record (-1 for unique)
	}

	var recs []*rec
	lookup := make(map[string]int) // folded string -> record index
	seeks := make([]int32, len(tracks))

	for id, tr := range tracks {
		limit := maxStringTagLen
		if tag == tagFilename {
			limit = maxPathTagLen
		}
		s := truncateTag(tagValue(tr, tag), limit)
		if unique {
			key := foldLower(s)
			if ri, ok := lookup[key]; ok {
				recs[ri].idxIDs = append(recs[ri].idxIDs, id)
				continue
			}
			lookup[key] = len(recs)
			recs = append(recs, &rec{str: s, idxIDs: []int{id}})
		} else {
			recs = append(recs, &rec{str: s, idxIDs: []int{id}})
		}
	}

	sort.SliceStable(recs, func(i, j int) bool {
		return tagCompare(recs[i].str, recs[j].str) < 0
	})

	path := filepath.Join(dir, fmt.Sprintf("database_%d.tcd", tag))
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)

	// Reserve the header so record data starts at offset 12; the real
	// header (with final datasize/entry_count) is patched in below.
	if _, err := w.Write(make([]byte, 12)); err != nil {
		return nil, err
	}

	var datasize uint32
	hdr := make([]byte, 12)
	putU32le(hdr[0:], tcMagic)
	for _, r := range recs {
		// tag_seek must be the absolute file offset (firmware compares
		// it against lseek() positions, i.e. header included).
		offset := int32(12 + datasize)
		data := append([]byte(r.str), 0)
		if tag != tagFilename {
			pad := (chunkLen - (len(data)+8)%chunkLen) % chunkLen
			data = append(data, make([]byte, pad)...)
			for i := len(data) - pad; i < len(data); i++ {
				data[i] = 'X'
			}
		}
		if int64(datasize)+int64(8+len(data)) > maxI32 {
			return nil, fmt.Errorf("database_%d.tcd would exceed 2 GiB", tag)
		}
		idxID := int32(-1)
		if !unique && len(r.idxIDs) > 0 {
			idxID = int32(r.idxIDs[0])
		}
		var fe [8]byte
		putU32le(fe[0:], uint32(len(data)))
		putU32le(fe[4:], uint32(idxID))
		if _, err := w.Write(fe[:]); err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		datasize += uint32(8 + len(data))

		for _, id := range r.idxIDs {
			seeks[id] = offset
		}
	}

	entryCount := len(recs)
	if tag == tagFilename {
		entryCount = len(tracks)
	}
	putU32le(hdr[4:], datasize)
	putU32le(hdr[8:], uint32(entryCount))
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if _, err := f.WriteAt(hdr, 0); err != nil {
		return nil, err
	}
	return seeks, f.Close()
}

func truncateTag(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.ValidString(s[:max]) {
		max--
	}
	return s[:max]
}

// TagProgress reports per-output-file progress during Build. tag is one of
// the tag constants, or -1 for the master index. done is true when the
// file has been written. May be nil.
type TagProgress func(tag int, done bool)

// Build writes the complete tagcache database for tracks into dir,
// reporting progress via onTag if non-nil.
func Build(dir string, tracks []*Track, onTag TagProgress) error {
	n := len(tracks)

	entries := make([]indexEntry, n)
	for i, tr := range tracks {
		for _, tag := range []int{tagYear, tagDiscNumber, tagTrackNumber,
			tagBitrate, tagLength, tagMtime, tagPlaycount, tagRating,
			tagPlaytime, tagLastPlayed, tagLastElapsed, tagLastOffset} {
			entries[i].seek[tag] = tagNumeric(tr, tag)
		}
	}

	var strDataSize int64
	for _, tag := range stringTags {
		if tag == tagFilename {
			continue
		}
		if onTag != nil {
			onTag(tag, false)
		}
		seeks, err := writeTagFile(dir, tag, tracks, tag != tagTitle)
		if err != nil {
			return fmt.Errorf("database_%d.tcd: %w", tag, err)
		}
		if onTag != nil {
			onTag(tag, true)
		}
		for i := range entries {
			entries[i].seek[tag] = seeks[i]
		}
		st, err := os.Stat(filepath.Join(dir, fmt.Sprintf("database_%d.tcd", tag)))
		if err != nil {
			return err
		}
		strDataSize += int64(st.Size()) - 12
	}

	if total := int64(24) + int64(96*n) + strDataSize; total > maxI32 {
		return fmt.Errorf("master index would exceed 2 GiB (%d bytes)", total)
	}

	fnSeeks, err := writeTagFile(dir, tagFilename, tracks, false)
	if err != nil {
		return fmt.Errorf("database_4.tcd: %w", err)
	}
	for i := range entries {
		entries[i].seek[tagFilename] = fnSeeks[i]
	}

	if onTag != nil {
		onTag(-1, false)
	}
	path := filepath.Join(dir, "database_idx.tcd")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)

	datasize := uint32(24 + 96*n + int(strDataSize))
	mh := make([]byte, 24)
	putU32le(mh[0:], tcMagic)
	putU32le(mh[4:], datasize)
	putU32le(mh[8:], uint32(n))
	putU32le(mh[12:], 0) // serial
	putU32le(mh[16:], 1) // commitid
	putU32le(mh[20:], 0) // dirty
	if _, err := w.Write(mh); err != nil {
		return err
	}

	buf := make([]byte, 4)
	for i := range entries {
		for j := 0; j < tagCount; j++ {
			putU32le(buf, uint32(entries[i].seek[j]))
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
		putU32le(buf, uint32(entries[i].flag))
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if onTag != nil {
		onTag(-1, true)
	}
	return nil
}

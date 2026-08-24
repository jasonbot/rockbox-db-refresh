package db

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"rbdb/internal/meta"
)

// flagDeleted mirrors FLAG_DELETED in apps/tagcache.c.
const flagDeleted = 0x0001

// ReadDatabase loads the tracks of an existing database from rbDir,
// reconstructing string metadata, numeric tags and runtime statistics.
func ReadDatabase(rbDir string) ([]*meta.Track, error) {
	idx, err := os.ReadFile(filepath.Join(rbDir, "database_idx.tcd"))
	if err != nil {
		return nil, err
	}
	if len(idx) < 24 || binary.LittleEndian.Uint32(idx[0:]) != tcMagic {
		return nil, fmt.Errorf("bad master index header")
	}
	entryCount := int(binary.LittleEndian.Uint32(idx[8:]))
	if entryCount < 0 || len(idx) < 24+96*entryCount {
		return nil, fmt.Errorf("truncated master index")
	}

	tagData := make(map[int][]byte, len(stringTags))
	for _, tag := range stringTags {
		data, err := os.ReadFile(filepath.Join(rbDir, fmt.Sprintf("database_%d.tcd", tag)))
		if err != nil {
			return nil, fmt.Errorf("database_%d.tcd: %w", tag, err)
		}
		if len(data) < 12 || binary.LittleEndian.Uint32(data[0:]) != tcMagic {
			return nil, fmt.Errorf("database_%d.tcd: bad header", tag)
		}
		tagData[tag] = data
	}

	tracks := make([]*meta.Track, 0, entryCount)
	for i := 0; i < entryCount; i++ {
		rec := idx[24+96*i:]
		var seek [tagCount]int32
		for j := range seek {
			seek[j] = int32(binary.LittleEndian.Uint32(rec[4*j:]))
		}
		flag := int32(binary.LittleEndian.Uint32(rec[4*tagCount:]))
		if flag&flagDeleted != 0 {
			continue
		}

		str := func(tag int) string { return tagStringAt(tagData[tag], seek[tag]) }

		devPath := str(tagFilename)
		if devPath == "" || devPath == untagged {
			return nil, fmt.Errorf("master index entry %d has no filename", i)
		}

		tracks = append(tracks, &meta.Track{
			DevPath: devPath,
			Meta: meta.Meta{
				Title:       str(tagTitle),
				Artist:      str(tagArtist),
				Album:       str(tagAlbum),
				Genre:       str(tagGenre),
				Composer:    str(tagComposer),
				Comment:     str(tagComment),
				AlbumArtist: str(tagAlbumArtist),
				Grouping:    str(tagGrouping),
			},
			Year:        int(seek[tagYear]),
			Disc:        int(seek[tagDiscNumber]),
			TrackNum:    int(seek[tagTrackNumber]),
			LengthMS:    int64(seek[tagLength]),
			Bitrate:     int(seek[tagBitrate]),
			MTime:       int64(seek[tagMtime]),
			Playcount:   int64(seek[tagPlaycount]),
			Rating:      int64(seek[tagRating]),
			Playtime:    int64(seek[tagPlaytime]),
			LastPlayed:  int64(seek[tagLastPlayed]),
			LastElapsed: int64(seek[tagLastElapsed]),
			LastOffset:  int64(seek[tagLastOffset]),
		})
	}
	return tracks, nil
}

// tagStringAt extracts the NUL-terminated string of the record at absolute
// file offset off (which points at the record's length field, matching how
// writeTagFile computes seek offsets).
func tagStringAt(data []byte, off int32) string {
	o := int(off)
	if o < 12 || o+8 > len(data) {
		return ""
	}
	n := int(binary.LittleEndian.Uint32(data[o:]))
	end := o + 8 + n
	if n < 1 || end > len(data) {
		return ""
	}
	raw := data[o+8 : end]
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	return string(raw)
}

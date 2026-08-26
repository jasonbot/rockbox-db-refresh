package meta

import (
	"encoding/binary"
	"io"
	"os"
	"strings"
)

// ---- Ogg (Vorbis / Opus) --------------------------------------------------

type oggPage struct {
	granule  int64
	segments [][]byte
}

func readOggPage(f *os.File) (*oggPage, error) {
	var h [27]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return nil, err
	}
	if string(h[:4]) != "OggS" || h[4] != 0 {
		return nil, errUnsupported
	}
	nseg := int(h[26])
	segtab := make([]byte, nseg)
	if _, err := io.ReadFull(f, segtab); err != nil {
		return nil, err
	}
	p := &oggPage{granule: int64(binary.LittleEndian.Uint64(h[6:14]))}
	for i := 0; i < nseg; i++ {
		seg := make([]byte, int(segtab[i]))
		if _, err := io.ReadFull(f, seg); err != nil {
			return nil, err
		}
		p.segments = append(p.segments, seg)
	}
	return p, nil
}

func pageComplete(p *oggPage) bool {
	if len(p.segments) == 0 {
		return true
	}
	last := p.segments[len(p.segments)-1]
	return len(last) < 255
}

// collectOggPackets reassembles the first count logical packets from the
// beginning of the stream.
func collectOggPackets(f *os.File, count int) [][]byte {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	var packets [][]byte
	var cur []byte
	inPacket := false
	for len(packets) < count+1 {
		page, err := readOggPage(f)
		if err != nil {
			break
		}
		for _, seg := range page.segments {
			if !inPacket {
				cur = append([]byte(nil), seg...)
				inPacket = true
			} else {
				cur = append(cur, seg...)
			}
			if len(seg) < 255 { // packet ended
				packets = append(packets, cur)
				cur = nil
				inPacket = false
				if len(packets) >= count {
					break
				}
			}
		}
		if !pageComplete(page) && inPacket {
			continue // packet continues on next page
		}
		if pageComplete(page) && !inPacket && len(packets) >= count {
			break
		}
	}
	return packets
}

func oggLastGranule(f *os.File) int64 {
	const tailMax = 1 << 20
	st, err := f.Stat()
	if err != nil {
		return -1
	}
	read := int64(tailMax)
	if st.Size() < read {
		read = st.Size()
	}
	buf := make([]byte, read)
	if _, err := f.ReadAt(buf, st.Size()-read); err != nil {
		return -1
	}
	for i := len(buf) - 27; i >= 0; i-- {
		// Accept any page header type (continuation/BOS/EOS flags live
		// in buf[i+5]); require segments present so stray 'OggS' text
		// inside packet data is unlikely to match.
		if string(buf[i:i+4]) == "OggS" && buf[i+4] == 0 && buf[i+26] > 0 {
			g := int64(binary.LittleEndian.Uint64(buf[i+6 : i+14]))
			if g >= 0 {
				return g
			}
		}
	}
	return -1
}

func parseOgg(f *os.File, t *Track, opus bool) error {
	packets := collectOggPackets(f, 2)
	if len(packets) < 2 {
		return errUnsupported
	}

	var lengthMS int64
	if opus {
		idh := packets[0]
		if len(idh) < 19 || string(idh[:8]) != "OpusHead" {
			return errUnsupported
		}
		preSkip := binary.LittleEndian.Uint16(idh[10:12])
		srate := binary.LittleEndian.Uint32(idh[12:16])
		tags := packets[1]
		if len(tags) > 8 && string(tags[:8]) == "OpusTags" {
			parseVorbisComments(tags[8:], t)
		}
		g := oggLastGranule(f)
		if g > int64(preSkip) && srate > 0 {
			lengthMS = (g - int64(preSkip)) * 1000 / int64(srate)
		}
	} else {
		idh := packets[0]
		if len(idh) < 28 || idh[0] != 1 || string(idh[1:7]) != "vorbis" {
			return errUnsupported
		}
		vrate := binary.LittleEndian.Uint32(idh[12:16])
		tags := packets[1]
		if len(tags) > 7 && tags[0] == 3 && string(tags[1:7]) == "vorbis" {
			parseVorbisComments(tags[7:], t)
		}
		g := oggLastGranule(f)
		if g > 0 && vrate > 0 {
			lengthMS = g * 1000 / int64(vrate)
		}
	}
	t.LengthMS = lengthMS
	t.Bitrate = bitrateFromSize(t.Size, lengthMS)
	if t.Bitrate > 500 {
		t.Bitrate = 0
	}
	return nil
}

// ---- MP4 -----------------------------------------------------------------

var mp4TagMap = map[string]string{
	"\xa9nam": "title", "\xa9ART": "artist", "\xa9alb": "album",
	"aART": "albumartist", "\xa9gen": "genre",
	"\xa9wrt": "composer", "\xa9cmt": "comment", "\xa9grp": "grouping",
	"\xa9day": "day", "gnre": "genre", "trkn": "", "disk": "",
	"covr": "",
}

func parseMP4(f *os.File, t *Track) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	mp4Walk(f, 0, st.Size(), 0, func(ftype string, start, size int64) bool {
		switch ftype {
		case "moov":
			mp4Walk(f, start+8, start+size, 1, func(t2 string, s2, z2 int64) bool {
				switch t2 {
				case "mvhd":
					mp4ParseMvhd(f, s2, z2, t)
				case "udta":
					mp4Walk(f, s2+8, s2+z2, 1, func(t3 string, s3, z3 int64) bool {
						if t3 == "meta" {
							mp4Walk(f, s3+12, s3+z3, 1, func(t4 string, s4, z4 int64) bool {
								if t4 == "ilst" {
									mp4ParseIlst(f, s4, z4, t)
									return false
								}
								return true
							})
						}
						return true
					})
				}
				return true
			})
			return false
		case "mdat":
			// Skip the media payload but keep walking; many muxers
			// place moov after mdat.
			return true
		}
		return true
	})
	if t.LengthMS == 0 && t.Size > 0 {
		return errUnsupported
	}
	return nil
}

func mp4Walk(f *os.File, from, to int64, depth int, fn func(string, int64, int64) bool) error {
	if depth > 10 || to-from < 8 {
		return nil
	}
	hdr := make([]byte, 16)
	pos := from
	for pos+8 <= to {
		if _, err := f.ReadAt(hdr[:8], pos); err != nil {
			return nil
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		ftype := string(hdr[4:8])
		if size == 1 {
			if _, err := f.ReadAt(hdr, pos); err != nil {
				return nil
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
		} else if size == 0 {
			size = to - pos
		}
		if size < 8 || pos+size > to {
			return nil
		}
		if !fn(ftype, pos, size) {
			return nil
		}
		pos += size
	}
	return nil
}

func mp4ParseMvhd(f *os.File, boxStart, boxSize int64, t *Track) {
	b := make([]byte, 32)
	if _, err := f.ReadAt(b, boxStart+8); err != nil {
		return
	}
	var timescale uint64
	var duration uint64
	if b[0] == 1 {
		timescale = uint64(binary.BigEndian.Uint32(b[20:24]))
		duration = binary.BigEndian.Uint64(b[24:32])
	} else if b[0] == 0 {
		timescale = uint64(binary.BigEndian.Uint32(b[12:16]))
		duration = uint64(binary.BigEndian.Uint32(b[16:20]))
	} else {
		return
	}
	if timescale > 0 && duration > 0 {
		t.LengthMS = int64(duration) * 1000 / int64(timescale)
		t.Bitrate = bitrateFromSize(t.Size, t.LengthMS)
	}
}

func mp4ParseIlst(f *os.File, boxStart, boxSize int64, t *Track) {
	mp4Walk(f, boxStart+8, boxStart+boxSize, 0, func(ftype string, start, size int64) bool {
		key, known := mp4TagMap[ftype]
		if !known {
			return true
		}
		var payload []byte
		mp4Walk(f, start+8, start+size, 0, func(c string, cs, cz int64) bool {
			if c == "data" && cz >= 17 {
				b := make([]byte, cz-16)
				if _, err := f.ReadAt(b, cs+16); err == nil {
					payload = b
				}
				return false
			}
			return true
		})
		if payload == nil {
			return true
		}
		val := strings.TrimSpace(string(payload))
		switch ftype {
		case "trkn":
			if len(payload) >= 4 {
				t.TrackNum = int(binary.BigEndian.Uint16(payload[2:4]))
			}
		case "disk":
			if len(payload) >= 4 {
				t.Disc = int(binary.BigEndian.Uint16(payload[2:4]))
			}
		case "\xa9day":
			if t.Year == 0 {
				t.Year = extractYear(val)
			}
		case "gnre":
			if len(payload) >= 2 {
				if g, ok := genreName(int(binary.BigEndian.Uint16(payload)) - 1); ok {
					t.Meta.Genre = or(t.Meta.Genre, g)
				}
			}
		case "covr":
			if len(payload) > 0 {
				t.CoverArt = payload
				if len(payload) >= 2 && payload[0] == 0xff && payload[1] == 0xd8 {
					t.CoverArtMIME = "image/jpeg"
				} else if len(payload) >= 4 && payload[0] == 0x89 && payload[1] == 'P' && payload[2] == 'N' && payload[3] == 'G' {
					t.CoverArtMIME = "image/png"
				} else {
					t.CoverArtMIME = "image/jpeg"
				}
			}
		default:
			setMetaField(&t.Meta, key, val)
		}
		return true
	})
}

func setMetaField(m *Meta, key, val string) {
	if val == "" {
		return
	}
	switch key {
	case "title":
		m.Title = or(m.Title, val)
	case "artist":
		m.Artist = or(m.Artist, val)
	case "album":
		m.Album = or(m.Album, val)
	case "albumartist":
		m.AlbumArtist = or(m.AlbumArtist, val)
	case "genre":
		m.Genre = or(m.Genre, normalizeGenre(val))
	case "composer":
		m.Composer = or(m.Composer, val)
	case "comment":
		m.Comment = or(m.Comment, val)
	case "grouping":
		m.Grouping = or(m.Grouping, val)
	}
}

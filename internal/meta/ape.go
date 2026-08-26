package meta

import (
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"strings"
)

// ---- APEv2 ---------------------------------------------------------------

func parseAPEv2File(f *os.File, t *Track) error {
	if t.Size < 64 {
		return nil
	}
	buf := make([]byte, 192)
	off := t.Size - int64(len(buf))
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]

	idx := strings.LastIndex(string(buf), "APETAGEX")
	if idx < 0 {
		return nil
	}
	foot := buf[idx:]
	tagSize := binary.LittleEndian.Uint32(foot[12:])
	itemCount := binary.LittleEndian.Uint32(foot[16:])
	flags := binary.LittleEndian.Uint32(foot[20:])
	if tagSize < 32 || itemCount == 0 || itemCount > 10000 {
		return nil
	}
	itemsStart := off + int64(idx) - (int64(tagSize) - 32)
	if flags&0x80000000 != 0 { // header present in size
		itemsStart += 32
	}
	if itemsStart < 0 || itemsStart >= t.Size {
		return nil
	}

	data := make([]byte, int64(tagSize)-32)
	if _, err := f.ReadAt(data, itemsStart); err != nil && err != io.EOF {
		return nil
	}

	pos := 0
	for i := uint32(0); i < itemCount && pos+8 <= len(data); i++ {
		vlen := int(binary.LittleEndian.Uint32(data[pos:]))
		pos += 8 // vlen + flags
		end := pos
		for end < len(data) && data[end] != 0 {
			end++
		}
		key := string(data[pos:end])
		valStart := end + 1
		if valStart+vlen > len(data) || vlen <= 0 {
			break
		}
		val := strings.TrimSpace(string(data[valStart : valStart+vlen]))
		pos = valStart + vlen

		switch strings.ToLower(key) {
		case "title":
			t.Meta.Title = or(t.Meta.Title, val)
		case "artist":
			t.Meta.Artist = or(t.Meta.Artist, val)
		case "album":
			t.Meta.Album = or(t.Meta.Album, val)
		case "album artist", "albumartist":
			t.Meta.AlbumArtist = or(t.Meta.AlbumArtist, val)
		case "genre", "genre ":
			t.Meta.Genre = or(t.Meta.Genre, normalizeGenre(val))
		case "composer":
			t.Meta.Composer = or(t.Meta.Composer, val)
		case "comment":
			t.Meta.Comment = or(t.Meta.Comment, val)
		case "grouping", "content group":
			t.Meta.Grouping = or(t.Meta.Grouping, val)
		case "track", "tracknumber":
			if n, ok := slashNum(val); ok && n > 0 {
				t.TrackNum = n
			}
		case "disc", "discnumber":
			if n, ok := slashNum(val); ok && n > 0 {
				t.Disc = n
			}
		case "year":
			if t.Year == 0 {
				t.Year = extractYear(val)
			}
		}
	}
	return nil
}

// ---- FLAC ----------------------------------------------------------------

func parseFLAC(f *os.File, t *Track) error {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != "fLaC" {
		return errUnsupported
	}
	hdr := make([]byte, 4)
	si := make([]byte, 34)
	for {
		if _, err := io.ReadFull(f, hdr); err != nil {
			return nil
		}
		last := hdr[0]&0x80 != 0
		btype := hdr[0] & 0x7f
		blen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])

		switch btype {
		case 0: // STREAMINFO
			if blen < 34 {
				if last {
					return nil
				}
				continue
			}
			if _, err := io.ReadFull(f, si); err != nil {
				return nil
			}
			srate := int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
			total := int64(si[13]&0x0f)<<32 | int64(si[14])<<24 |
				int64(si[15])<<16 | int64(si[16])<<8 | int64(si[17])
			if srate > 0 && total > 0 {
				t.LengthMS = total * 1000 / int64(srate)
				t.Bitrate = bitrateFromSize(t.Size, t.LengthMS)
			}
			if last {
				return nil
			}
			continue
		case 4: // VORBIS_COMMENT
			body := make([]byte, blen)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil
			}
			parseVorbisComments(body, t)
		default:
			if _, err := f.Seek(int64(blen), io.SeekCurrent); err != nil {
				return nil
			}
		}
		if last {
			return nil
		}
	}
}

func parseVorbisComments(body []byte, t *Track) {
	pos := 0
	u32 := func() (uint32, bool) {
		if pos+4 > len(body) {
			return 0, false
		}
		v := binary.LittleEndian.Uint32(body[pos:])
		pos += 4
		return v, true
	}
	vlen, ok := u32()
	if !ok || pos+int(vlen) > len(body) {
		return
	}
	pos += int(vlen)
	count, ok := u32()
	if !ok || count > 100000 {
		return
	}
	for i := uint32(0); i < count && pos+4 <= len(body); i++ {
		l, ok := u32()
		if !ok || pos+int(l) > len(body) {
			return
		}
		kv := string(body[pos : pos+int(l)])
		pos += int(l)
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			key, val := kv[:eq], kv[eq+1:]
			if strings.ToUpper(key) == "METADATA_BLOCK_PICTURE" {
				// Value is base64-encoded binary data
				decoded, err := base64.StdEncoding.DecodeString(val)
				if err == nil {
					if pic, picMIME, ok := parseVorbisPictureBlock(decoded); ok {
						t.CoverArt = pic
						t.CoverArtMIME = picMIME
					}
				}
				continue
			}
			if !setVorbis(&t.Meta, key, val) {
				setNumericVorbis(t, key, val)
			}
		}
	}
}

func parseVorbisPictureBlock(data []byte) ([]byte, string, bool) {
	if len(data) < 32 {
		return nil, "", false
	}
	pos := 0
	u32 := func() uint32 {
		if pos+4 > len(data) {
			return 0
		}
		v := binary.LittleEndian.Uint32(data[pos:])
		pos += 4
		return v
	}

	_ = u32() // picture type
	mimeLen := int(u32())
	if pos+mimeLen > len(data) {
		return nil, "", false
	}
	mime := string(data[pos : pos+mimeLen])
	pos += mimeLen

	descLen := int(u32())
	pos += descLen
	if pos+16 > len(data) { // width, height, depth, colors
		return nil, "", false
	}
	pos += 16

	dataLen := int(u32())
	if pos+dataLen > len(data) || dataLen <= 0 {
		return nil, "", false
	}
	return data[pos : pos+dataLen], mime, true
}

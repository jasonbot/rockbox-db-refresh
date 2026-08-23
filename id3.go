package main

import (
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ---- ID3v2 ---------------------------------------------------------------

func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 |
		uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}

func deUnsync(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xff && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

func id3TextDecode(enc byte, raw []byte) string {
	switch enc {
	case 0:
		r := make([]rune, 0, len(raw))
		for _, c := range raw {
			if c == 0 {
				break
			}
			r = append(r, rune(c))
		}
		return strings.TrimSpace(string(r))
	case 1: // UTF-16 with BOM
		return utf16Decode(raw, true)
	case 2: // UTF-16BE, no BOM
		return utf16DecodeBE(raw)
	default: // 3: UTF-8
		if i := strings.IndexByte(string(raw), 0); i >= 0 {
			raw = raw[:i]
		}
		return strings.TrimSpace(string(raw))
	}
}

func utf16Decode(raw []byte, bom bool) string {
	be := false
	if bom && len(raw) >= 2 {
		if raw[0] == 0xfe && raw[1] == 0xff {
			be = true
			raw = raw[2:]
		} else if raw[0] == 0xff && raw[1] == 0xfe {
			raw = raw[2:]
		}
	} else if bom {
		return ""
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		var v uint16
		if be {
			v = binary.BigEndian.Uint16(raw[i:])
		} else {
			v = binary.LittleEndian.Uint16(raw[i:])
		}
		if v == 0 {
			break
		}
		u = append(u, v)
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

func utf16DecodeBE(raw []byte) string { return utf16Decode(raw, false) }

type id3Result struct {
	meta Meta
	set  map[string]bool
}

func (r *id3Result) setField(k, v string) {
	if v == "" {
		return
	}
	if r.set == nil {
		r.set = make(map[string]bool)
	}
	if r.set[k] {
		return
	}
	r.set[k] = true
	switch k {
	case "title":
		r.meta.Title = v
	case "artist":
		r.meta.Artist = v
	case "album":
		r.meta.Album = v
	case "albumartist":
		r.meta.AlbumArtist = v
	case "genre":
		r.meta.Genre = v
	case "composer":
		r.meta.Composer = v
	case "comment":
		r.meta.Comment = v
	case "grouping":
		r.meta.Grouping = v
	}
}

func parseID3v2(f *os.File, t *Track) (int64, bool) {
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil || string(hdr[:3]) != "ID3" {
		return 0, false
	}
	verMajor := int(hdr[3])
	flags := hdr[5]
	size := int64(syncsafe(hdr[6:10]))
	bodyOff := int64(10)
	if flags&0x10 != 0 { // footer present (v4)
		size += 10
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return bodyOff + size, false
	}

	if flags&0x40 != 0 { // extended header
		if verMajor == 4 && len(body) >= 4 {
			if n := int(syncsafe(body[:4])); n > 0 && n <= len(body) {
				body = body[n:]
			}
		} else if verMajor == 3 && len(body) >= 8 {
			if n := int(binary.BigEndian.Uint32(body[:4])); n > 0 &&
				int(n)+4 <= len(body) {
				body = body[4+n:]
			}
		}
	}
	if verMajor <= 3 && flags&0x80 != 0 {
		body = deUnsync(body)
	}

	res := &id3Result{}
	pos := 0
	var id string
	var fsize int
	var fflags uint16
	for {
		if verMajor == 2 {
			if pos+6 > len(body) {
				break
			}
			id = string(body[pos : pos+3])
			fsize = int(body[pos+3])<<16 | int(body[pos+4])<<8 | int(body[pos+5])
			pos += 6
		} else {
			if pos+10 > len(body) {
				break
			}
			id = string(body[pos : pos+4])
			if verMajor == 4 {
				fsize = int(syncsafe(body[pos+4 : pos+8]))
			} else {
				fsize = int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
			}
			fflags = binary.BigEndian.Uint16(body[pos+8 : pos+10])
			pos += 10
		}
		if fsize <= 0 || pos+fsize > len(body) {
			break
		}
		data := body[pos : pos+fsize]
		pos += fsize

		if verMajor >= 4 && fflags&0x02 != 0 {
			data = deUnsync(data)
		}
		if verMajor >= 4 && fflags&0x01 != 0 && len(data) >= 4 {
			data = data[4:] // data length indicator
		}
		if verMajor == 3 && fflags&0x00c0 != 0 {
			continue // compressed/encrypted frames unsupported
		}

		applyID3Frame(res, &t.Year, &t.TrackNum, &t.Disc, verMajor, id, data)
	}

	t.Meta.Title = or(res.meta.Title, t.Meta.Title)
	t.Meta.Artist = or(res.meta.Artist, t.Meta.Artist)
	t.Meta.Album = or(res.meta.Album, t.Meta.Album)
	t.Meta.AlbumArtist = or(res.meta.AlbumArtist, t.Meta.AlbumArtist)
	t.Meta.Genre = or(res.meta.Genre, t.Meta.Genre)
	t.Meta.Composer = or(res.meta.Composer, t.Meta.Composer)
	t.Meta.Comment = or(res.meta.Comment, t.Meta.Comment)
	t.Meta.Grouping = or(res.meta.Grouping, t.Meta.Grouping)

	return bodyOff + size, true
}

func applyID3Frame(res *id3Result, year *int, trk *int, disc *int, ver int, id string, data []byte) {
	if len(data) == 0 {
		return
	}
	isText := strings.HasPrefix(id, "T") && id != "TXXX"
	switch {
	case isText:
		v := id3TextDecode(data[0], data[1:])
		if ver == 4 {
			v = firstField(v) // NUL-separated multiple values
		}
		switch id {
		case "TIT2", "TT2":
			res.setField("title", v)
		case "TPE1", "TP1":
			res.setField("artist", v)
		case "TALB", "TAL":
			res.setField("album", v)
		case "TPE2", "TP2":
			res.setField("albumartist", v)
		case "TCOM", "TCM":
			res.setField("composer", v)
		case "TIT1", "TT1":
			res.setField("grouping", v)
		case "TCON", "TCO":
			res.setField("genre", normalizeGenre(v))
		case "TRCK", "TRK":
			if n, ok := slashNum(v); ok && n > 0 {
				*trk = n
			}
		case "TPOS", "TPA":
			if n, ok := slashNum(v); ok && n > 0 {
				*disc = n
			}
		case "TDRC", "TYER", "TYE", "TDAT", "TDA":
			if *year == 0 {
				*year = extractYear(v)
			}
		}
	case id == "COMM" || id == "COM":
		enc := data[0]
		rest := data[1:]
		if len(rest) < 4 {
			return
		}
		rest = rest[3:] // language
		var comment string
		if enc == 1 || enc == 2 {
			comment = utf16Decode(rest, true)
		} else {
			if i := strings.IndexByte(string(rest), 0); i >= 0 {
				rest = rest[:i]
			}
			comment = strings.TrimSpace(string(rest))
		}
		res.setField("comment", comment)
	}
}

var id3Genres = [...]string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge",
	"Hip-Hop", "Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B",
	"Rap", "Reggae", "Rock", "Techno", "Industrial", "Alternative", "Ska",
	"Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient",
	"Trip-Hop", "Vocal", "Jazz+Funk", "Fusion", "Trance", "Classical",
	"Instrumental", "Acid", "House", "Game", "Sound Clip", "Gospel", "Noise",
	"Alt. Rock", "Bass", "Soul", "Punk", "Space", "Meditative",
	"Instrumental Pop", "Instrumental Rock", "Ethnic", "Gothic", "Darkwave",
	"Techno-Industrial", "Electronic", "Pop-Folk", "Eurodance", "Dream",
	"Southern Rock", "Comedy", "Cult", "Gangsta Rap", "Top 40", "Christian Rap",
	"Pop/Funk", "Jungle", "Native American", "Cabaret", "New Wave",
	"Psychedelic", "Rave", "Showtunes", "Trailer", "Lo-Fi", "Tribal",
	"Acid Punk", "Acid Jazz", "Polka", "Retro", "Musical", "Rock & Roll",
	"Hard Rock", "Folk", "Folk-Rock", "National Folk", "Swing", "Fast-Fusion",
	"Bebop", "Latin", "Revival", "Celtic", "Bluegrass", "Avantgarde",
	"Gothic Rock", "Progressive Rock", "Psychedelic Rock", "Symphonic Rock",
	"Slow Rock", "Big Band", "Chorus", "Easy Listening", "Acoustic", "Humour",
	"Speech", "Chanson", "Opera", "Chamber Music", "Sonata", "Symphony",
	"Booty Bass", "Primus", "Porn Groove", "Satire", "Slow Jam", "Club",
	"Tango", "Samba", "Folklore", "Ballad", "Power Ballad", "Rhythmic Soul",
	"Freestyle", "Duet", "Punk Rock", "Drum Solo", "A Cappella", "Euro-House",
	"Dance Hall", "Goa", "Drum & Bass", "Club-House", "Hardcore", "Terror",
	"Indie", "BritPop", "Afro-Punk", "Polsk Punk", "Beat",
	"Christian Gangsta Rap", "Heavy Metal", "Black Metal", "Crossover",
	"Contemporary Christian", "Christian Rock", "Merengue", "Salsa",
	"Thrash Metal", "Anime", "JPop", "Synthpop",
}

func genreName(n int) (string, bool) {
	if n >= 0 && n < len(id3Genres) {
		return id3Genres[n], true
	}
	return "", false
}

func normalizeGenre(s string) string {
	s = strings.TrimSpace(s)
	for {
		if !strings.HasPrefix(s, "(") {
			break
		}
		end := strings.IndexByte(s, ')')
		if end < 0 {
			break
		}
		inner := s[1:end]
		if n, err := strconv.Atoi(inner); err == nil {
			if g, ok := genreName(n); ok {
				s = s[end+1:]
				if s == "" {
					return g
				}
				continue
			}
		}
		s = s[end+1:]
	}
	if n, err := strconv.Atoi(s); err == nil {
		if g, ok := genreName(n); ok {
			return g
		}
	}
	return s
}

// ---- ID3v1 ---------------------------------------------------------------

func parseID3v1(f *os.File, t *Track) {
	if _, err := f.Seek(-128, io.SeekEnd); err != nil {
		return
	}
	buf := make([]byte, 128)
	if _, err := io.ReadFull(f, buf); err != nil || string(buf[:3]) != "TAG" {
		return
	}
	str := func(a, b int) string {
		s := strings.TrimRight(string(buf[a:b]), "\x00 ")
		return strings.TrimSpace(s)
	}
	t.Meta.Title = or(t.Meta.Title, str(3, 33))
	t.Meta.Artist = or(t.Meta.Artist, str(33, 63))
	t.Meta.Album = or(t.Meta.Album, str(63, 93))
	if y := str(93, 97); t.Year == 0 {
		t.Year = extractYear(y)
	}
	if c := str(97, 125); c != "" {
		t.Meta.Comment = or(t.Meta.Comment, c)
	}
	if buf[125] == 0 && buf[126] != 0 { // ID3v1.1 track number
		if t.TrackNum < 0 {
			t.TrackNum = int(buf[126])
		}
	}
	if g, ok := genreName(int(buf[127])); ok && buf[127] != 255 && t.Meta.Genre == "" {
		t.Meta.Genre = g
	}
}

// ---- MP3 duration / bitrate ----------------------------------------------

var mp3Bitrates = [6][15]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}, // V1 L1
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},    // V1 L2
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},     // V1 L3
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},    // V2 L1
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},         // V2 L2
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},         // V2 L3
}

func mpegBitrate(idx int, version int, layer int) int {
	row := 0
	switch {
	case version == 3 && layer == 3:
		row = 0 // MPEG1 Layer I
	case version == 3 && layer == 2:
		row = 1
	case version == 3:
		row = 2
	case layer == 3:
		row = 3
	case layer == 2:
		row = 4
	default:
		row = 5
	}
	if idx <= 0 || idx > 14 {
		return 0
	}
	return mp3Bitrates[row][idx]
}

func mpegSamplesPerFrame(version int, layer int) int {
	switch {
	case version == 3 && layer == 1: // MPEG1 L3
		return 1152
	case version == 3 && layer == 2: // MPEG1 L2
		return 1152
	case version == 3: // MPEG1 L1
		return 384
	case layer == 1: // MPEG2/2.5 L3
		return 576
	case layer == 2:
		return 1152
	default:
		return 384
	}
}

// Index is the raw MPEG version field: 0=MPEG2.5, 1=reserved, 2=MPEG2,
// 3=MPEG1.
var mpegSampleRates = [4][3]int{
	{11025, 12000, 8000},  // MPEG2.5
	{},                    // reserved
	{22050, 24000, 16000}, // MPEG2
	{44100, 48000, 32000}, // MPEG1
}

func parseMP3(f *os.File, t *Track) error {
	audioStart, _ := parseID3v2(f, t)
	parseID3v1(f, t)

	buf := make([]byte, 8192)
	if _, err := f.Seek(audioStart, io.SeekStart); err != nil {
		return err
	}
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]

	findSync := func(from int) int {
		for i := from; i+4 <= len(buf); i++ {
			b := buf[i:]
			if b[0] != 0xff || b[1]&0xe0 != 0xe0 {
				continue
			}
			version := int(b[1]>>3) & 3 // 3=MPEG1, 2=MPEG2, 0=MPEG2.5
			layer := int(b[1]>>1) & 3   // 1=L3, 2=L2, 3=L1
			if version == 1 || layer == 0 {
				continue
			}
			brIdx := int(b[2] >> 4)
			srIdx := int(b[2]>>2) & 3
			if brIdx == 0 || brIdx == 15 || srIdx == 3 {
				continue
			}
			return i
		}
		return -1
	}

	p := findSync(0)
	if p < 0 {
		return nil
	}
	h := buf[p:]
	version := int(h[1]>>3) & 3
	layer := int(h[1]>>1) & 3
	brIdx := int(h[2] >> 4)
	srIdx := int(h[2]>>2) & 3
	padding := int(h[2]>>1) & 1
	channelMode := int(h[3] >> 6)

	bitrate := mpegBitrate(brIdx, version, layer)
	srTab := mpegSampleRates[version]
	if bitrate == 0 || srIdx >= 3 {
		return nil
	}
	srate := srTab[srIdx]
	if srate == 0 || layer == 0 {
		return nil
	}

	spf := mpegSamplesPerFrame(version, layer)
	frameLen := 0
	if version == 3 && layer == 3 { // MPEG1 Layer I
		frameLen = (12*bitrate*1000/srate + padding) * 4
	} else if version != 3 && layer == 3 {
		frameLen = (6*bitrate*1000/srate + padding) * 4
	} else {
		frameLen = 144 * bitrate * 1000 / srate
		if version != 3 {
			frameLen /= 2
		}
	}
	if frameLen <= 4 {
		return nil
	}

	audioSize := t.Size - audioStart

	// Xing/Info VBR header
	sideInfo := 17
	if version == 3 {
		if channelMode != 3 {
			sideInfo = 32
		}
	} else if channelMode != 3 {
		sideInfo = 9
	}
	xp := p + 4 + sideInfo
	durationMS := int64(0)
	if xp+16 <= len(buf) && (string(buf[xp:xp+4]) == "Xing" || string(buf[xp:xp+4]) == "Info") {
		xflags := binary.BigEndian.Uint32(buf[xp+4:])
		frames := uint32(0)
		if xflags&1 != 0 && xp+12 <= len(buf) {
			frames = binary.BigEndian.Uint32(buf[xp+8:])
		}
		if frames > 0 {
			durationMS = int64(frames) * int64(spf) * 1000 / int64(srate)
			if xflags&0x100 != 0 && xp+20 <= len(buf) {
				t.Bitrate = int(binary.BigEndian.Uint32(buf[xp+16:]))
			}
		}
	}
	if durationMS == 0 && bitrate > 0 {
		durationMS = audioSize * 8 / int64(bitrate)
	}
	t.LengthMS = durationMS
	if t.Bitrate == 0 {
		t.Bitrate = bitrateFromSize(audioSize, durationMS)
	}
	return nil
}

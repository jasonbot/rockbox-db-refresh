package meta

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Meta struct {
	Title, Artist, Album, AlbumArtist string
	Genre, Composer, Comment          string
	Grouping                          string
}

type Track struct {
	DevPath  string
	HostPath string
	Size     int64
	MTime    int64
	CTime    int64

	Meta     Meta
	Year     int
	Disc     int // 0 = unknown
	TrackNum int // -1 = unknown
	LengthMS int64
	Bitrate  int // kbps, 0 = unknown

	CoverArt     []byte // embedded cover art (raw image bytes)
	CoverArtMIME string // MIME type of CoverArt (e.g. "image/jpeg")

	// Runtime statistics; zero unless restored by -refresh from an
	// existing database.
	Playcount   int64
	Rating      int64
	Playtime    int64
	LastPlayed  int64
	LastElapsed int64
	LastOffset  int64

	// Added is when the track entered the library (unix seconds). Seeded
	// from the file ctime on first sight and kept stable across refreshes
	// via <root>/.rockbox/added.tsv; used by -shuffle recency weighting.
	Added int64
}

// AudioExts lists the file extensions the scanner picks up.
var AudioExts = map[string]bool{
	".mp3": true, ".mp2": true, ".flac": true,
	".ogg": true, ".oga": true, ".opus": true,
	".m4a": true, ".m4b": true, ".mp4": true,
	".ape": true, ".mpc": true, ".wv": true,
}

var errUnsupported = errors.New("unsupported format")

// ParseTrack probes hostPath, fills in a Track with device path devPath.
func ParseTrack(hostPath, devPath string) (*Track, error) {
	f, err := os.Open(hostPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	t := &Track{DevPath: devPath, HostPath: hostPath, Size: st.Size(),
		MTime: st.ModTime().Unix(), CTime: fileCTime(st), TrackNum: -1}

	switch strings.ToLower(filepath.Ext(hostPath)) {
	case ".mp3", ".mp2":
		err = parseMP3(f, t)
	case ".flac":
		err = parseFLAC(f, t)
	case ".ogg", ".oga":
		err = parseOgg(f, t, false)
	case ".opus":
		err = parseOgg(f, t, true)
	case ".m4a", ".m4b", ".mp4":
		err = parseMP4(f, t)
	case ".ape", ".mpc", ".wv":
		err = parseAPEv2File(f, t)
	default:
		err = errUnsupported
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func slashNum(s string) (int, bool) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

func extractYear(s string) int {
	for i := 0; i+4 <= len(s); i++ {
		y := s[i : i+4]
		if y[0] == '1' || y[0] == '2' {
			if n, err := strconv.Atoi(y); err == nil && n >= 1000 && n <= 2999 {
				return n
			}
		}
	}
	return 0
}

func firstField(s string) string {
	if i := strings.IndexAny(s, "\x00/"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func setVorbis(m *Meta, key, val string) bool {
	key = strings.ToUpper(key)
	val = strings.TrimSpace(val)
	if val == "" {
		return false
	}
	switch key {
	case "TITLE":
		m.Title = or(m.Title, val)
	case "ARTIST":
		m.Artist = or(m.Artist, val)
	case "ALBUM":
		m.Album = or(m.Album, val)
	case "ALBUMARTIST", "ALBUM ARTIST", "ALBUM_ARTIST":
		m.AlbumArtist = or(m.AlbumArtist, val)
	case "GENRE":
		m.Genre = or(m.Genre, val)
	case "COMPOSER":
		m.Composer = or(m.Composer, val)
	case "COMMENT", "DESCRIPTION":
		m.Comment = or(m.Comment, val)
	case "GROUPING", "CONTENTGROUP", "CONTENT GROUP":
		m.Grouping = or(m.Grouping, val)
	default:
		return false
	}
	return true
}

func setNumericVorbis(t *Track, key, val string) {
	key = strings.ToUpper(key)
	switch key {
	case "TRACKNUMBER":
		if n, ok := slashNum(val); ok && n > 0 {
			t.TrackNum = n
		}
	case "DISCNUMBER":
		if n, ok := slashNum(val); ok && n > 0 {
			t.Disc = n
		}
	case "DATE", "YEAR", "ORIGINALDATE", "ORIGINALYEAR":
		if t.Year == 0 {
			t.Year = extractYear(val)
		}
	}
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// bitrateFromSize estimates average kbps from size (bytes) and duration (ms).
func bitrateFromSize(size int64, lengthMS int64) int {
	if lengthMS <= 0 {
		return 0
	}
	kbits := size * 8 / 1000
	secs := lengthMS / 1000
	if secs <= 0 {
		return 0
	}
	return int(kbits / secs)
}

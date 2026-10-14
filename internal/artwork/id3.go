package artwork

import (
	"bytes"
	"encoding/binary"
	"os"
)

// ExtractMP3Cover returns the front-cover image bytes from an MP3's ID3v2
// APIC frame (JPEG or PNG), or nil when the file carries no usable picture.
// ID3v2.3 and v2.4 frame layouts are both understood; tag-level and
// frame-level unsynchronisation are undone before parsing.
func ExtractMP3Cover(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hdr [10]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return nil, nil // unreadable header: treat as artless
	}
	if string(hdr[:3]) != "ID3" {
		return nil, nil
	}
	ver := hdr[3]
	flags := hdr[5]
	tagSize := syncsafe(hdr[6:10])
	if tagSize < 0 || tagSize > 64<<20 {
		return nil, nil
	}
	tag := make([]byte, tagSize)
	if _, err := f.ReadAt(tag, 10); err != nil {
		return nil, nil
	}
	if flags&0x80 != 0 && ver < 4 { // tag-level unsynchronisation (v2.3)
		tag = deunsync(tag)
	}

	pos := 0
	if flags&0x40 != 0 { // extended header
		var ext int
		if ver == 4 {
			ext = syncsafe(tag[0:4])
		} else {
			ext = int(binary.BigEndian.Uint32(tag[0:4])) + 4
		}
		if ext > 0 && ext < len(tag) {
			pos = ext
		}
	}

	var first []byte
	for pos+10 <= len(tag) {
		id := string(tag[pos : pos+4])
		if id == "\x00\x00\x00\x00" {
			break
		}
		var size int
		if ver == 4 {
			size = syncsafe(tag[pos+4 : pos+8])
		} else {
			size = int(binary.BigEndian.Uint32(tag[pos+4 : pos+8]))
		}
		if size <= 0 || pos+10+size > len(tag) {
			break
		}
		body := tag[pos+10 : pos+10+size]
		if ver == 4 && tag[pos+9]&0x02 != 0 { // frame-level unsynchronisation
			body = deunsync(body)
		}
		if id == "APIC" {
			if img, ptype := parseAPIC(body); img != nil {
				if ptype == 3 { // front cover wins outright
					return img, nil
				}
				if first == nil {
					first = img
				}
			}
		}
		pos += 10 + size
	}
	return first, nil
}

// parseAPIC splits an APIC frame body into the embedded image and the
// picture type (3 = front cover). Returns nil when no image is present.
func parseAPIC(b []byte) (img []byte, ptype byte) {
	if len(b) < 4 {
		return nil, 0
	}
	enc := b[0]
	rest := b[1:]
	// MIME: NUL-terminated ASCII.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, 0
	}
	rest = rest[i+1:]
	if len(rest) < 1 {
		return nil, 0
	}
	ptype = rest[0]
	rest = rest[1:]
	// Description: NUL-terminated; UTF-16 variants use a double-NUL on an
	// even boundary, ISO-8859-1/UTF-8 a single NUL.
	switch enc {
	case 1, 2: // UTF-16 (with BOM) / UTF-16BE
		j := 0
		for j+1 < len(rest) {
			if rest[j] == 0 && rest[j+1] == 0 {
				break
			}
			j += 2
		}
		rest = rest[min(j+2, len(rest)):]
	default:
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return nil, ptype
		}
		rest = rest[j+1:]
	}
	if len(rest) < 4 {
		return nil, ptype
	}
	return rest, ptype
}

func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

// deunsync removes the 0x00 bytes inserted after 0xFF by unsynchronisation.
func deunsync(b []byte) []byte {
	if !bytes.Contains(b, []byte{0xFF, 0x00}) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

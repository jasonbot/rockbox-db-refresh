package artwork

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/jpeg" // register JPEG for image.Decode
	_ "image/png"  // register PNG for image.Decode

	"golang.org/x/image/draw"
)

// Format describes one ithmb image format the device expects.
// The Classic uses square RGB565 little-endian formats with stride == width.
type Format struct {
	ID     int // correlation id stored in mhni/mhif and the ithmb file name
	Width  int
	Height int
}

// EntrySize is the per-image byte size of this format inside the ithmb file.
func (f Format) EntrySize() int { return f.Width * f.Height * 2 }

// ClassicFormats are the three cover-art formats iTunes writes for the
// iPod Classic: list thumbnail, medium, and full-screen Cover Flow.
func ClassicFormats() []Format {
	return []Format{
		{ID: 1061, Width: 56, Height: 56},
		{ID: 1055, Width: 128, Height: 128},
		{ID: 1060, Width: 320, Height: 320},
	}
}

// ToRGB565 decodes a JPEG/PNG image, resizes it to exactly w×h (album art is
// square by convention, matching iTunes), and packs it as RGB565
// little-endian pixels: r>>3<<11 | g>>2<<5 | b>>3.
func ToRGB565(data []byte, w, h int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	if bounds.Dx() != w || bounds.Dy() != h {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
		img = dst
	}
	out := make([]byte, w*h*2)
	i := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			v := uint16(r>>11&0x1F)<<11 | uint16(g>>10&0x3F)<<5 | uint16(b>>11&0x1F)
			binary.LittleEndian.PutUint16(out[i:], v)
			i += 2
		}
	}
	return out, nil
}

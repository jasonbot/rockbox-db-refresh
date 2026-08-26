package artwork

import (
	"bytes"
	"image"
	"image/jpeg"

	"golang.org/x/image/draw"
)

const (
	DefaultMaxDim       = 300
	DefaultMaxArtFileSize = 500 * 1024 // 500KB
)

func ProcessCoverArt(data []byte, maxDim, maxFileSize int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w > maxDim || h > maxDim {
		newW, newH := w, h
		if w > h {
			newW = maxDim
			newH = h * maxDim / w
		} else {
			newH = maxDim
			newW = w * maxDim / h
		}
		dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
		img = dst
	}

	quality := 95
	for quality >= 50 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if buf.Len() <= maxFileSize {
			return buf.Bytes(), nil
		}
		quality -= 10
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

package artwork

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func testJPEG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeMP3WithArt(t *testing.T, path string, art []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("FAKEMP3AUDIODATA"), 0644); err != nil {
		t.Fatal(err)
	}
	if art != nil {
		if err := EmbedCoverArtToMP3(path, art, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractMP3Cover(t *testing.T) {
	dir := t.TempDir()
	art := testJPEG(t, 8, 8, color.RGBA{200, 30, 30, 255})
	p := filepath.Join(dir, "a.mp3")
	writeMP3WithArt(t, p, art)

	got, err := ExtractMP3Cover(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, art) {
		t.Fatalf("extracted %d bytes, want the embedded %d", len(got), len(art))
	}

	// No tag at all.
	p2 := filepath.Join(dir, "b.mp3")
	writeMP3WithArt(t, p2, nil)
	got, err = ExtractMP3Cover(p2)
	if err != nil || got != nil {
		t.Fatalf("artless file: got %d bytes, err %v", len(got), err)
	}
}

func TestToRGB565(t *testing.T) {
	art := testJPEG(t, 32, 32, color.RGBA{255, 0, 0, 255})
	pix, err := ToRGB565(art, 56, 56)
	if err != nil {
		t.Fatal(err)
	}
	if len(pix) != 56*56*2 {
		t.Fatalf("size = %d", len(pix))
	}
	// Red channel dominates: R top 5 bits set, G/B ~0 (allow JPEG wobble).
	v := uint16(pix[0]) | uint16(pix[1])<<8
	if v>>11 < 30 || v&0x07FF != 0 {
		t.Fatalf("first pixel = %#x, want ~0xF800", v)
	}
}

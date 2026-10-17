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

func TestExtractMP3CoverSkipsCorruptPayload(t *testing.T) {
	dir := t.TempDir()
	art := testJPEG(t, 8, 8, color.RGBA{30, 200, 30, 255})

	// Build an ID3v2.3 tag by hand: one APIC with a garbage payload
	// (as left by broken taggers) followed by a valid APIC.
	apic := func(payload []byte) []byte {
		body := append([]byte{0}, []byte("image/jpeg")...)
		body = append(body, 0, 3, 0) // enc, mime NUL, type=front, desc NUL
		body = append(body, payload...)
		out := []byte("APIC")
		var sz [4]byte
		sz[0] = byte(len(body) >> 24)
		sz[1] = byte(len(body) >> 16)
		sz[2] = byte(len(body) >> 8)
		sz[3] = byte(len(body))
		out = append(out, sz[:]...)
		out = append(out, 0, 0) // flags
		return append(out, body...)
	}
	tag := append(apic([]byte("o\x00v\x00e\x00r\x00garbage")), apic(art)...)
	hdr := []byte("ID3")
	hdr = append(hdr, 3, 0, 0)
	hdr = append(hdr, byte(len(tag)>>21)&0x7f, byte(len(tag)>>14)&0x7f, byte(len(tag)>>7)&0x7f, byte(len(tag))&0x7f)

	p := filepath.Join(dir, "broken.mp3")
	data := append(append(hdr, tag...), []byte("AUDIO")...)
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := ExtractMP3Cover(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, art) {
		t.Fatalf("got %d bytes, want the valid APIC (%d bytes)", len(got), len(art))
	}
}

func TestExtractMP3CoverCorruptOnly(t *testing.T) {
	dir := t.TempDir()
	body := append([]byte{0}, []byte("image/jpeg")...)
	body = append(body, 0, 3, 0)
	body = append(body, []byte("not an image at all")...)
	tag := []byte("APIC")
	tag = append(tag, byte(len(body)>>24), byte(len(body)>>16), byte(len(body)>>8), byte(len(body)), 0, 0)
	tag = append(tag, body...)
	tag = append(tag, body...)
	hdr := []byte("ID3")
	hdr = append(hdr, 3, 0, 0, byte(len(tag)>>21)&0x7f, byte(len(tag)>>14)&0x7f, byte(len(tag)>>7)&0x7f, byte(len(tag))&0x7f)
	p := filepath.Join(dir, "broken2.mp3")
	if err := os.WriteFile(p, append(append(hdr, tag...), []byte("AUDIO")...), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractMP3Cover(p)
	if err != nil || got != nil {
		t.Fatalf("corrupt-only APIC: got %d bytes, err %v; want nil, nil", len(got), err)
	}
}

func TestLooksLikeImage(t *testing.T) {
	if !LooksLikeImage([]byte{0xFF, 0xD8, 0xFF, 0xE0}) {
		t.Fatal("jpeg not detected")
	}
	if !LooksLikeImage([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		t.Fatal("png not detected")
	}
	if LooksLikeImage([]byte("plain text")) {
		t.Fatal("text detected as image")
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

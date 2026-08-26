package artwork

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

func EmbedCoverArtToMP3(mp3Path string, coverArt []byte, mimeType string) error {
	data, err := os.ReadFile(mp3Path)
	if err != nil {
		return err
	}

	var frame bytes.Buffer
	frame.WriteString("APIC")

	var body bytes.Buffer
	body.WriteByte(0)
	body.WriteString(mimeType)
	body.WriteByte(0)
	body.WriteByte(3)
	body.WriteByte(0)
	body.Write(coverArt)

	bodyBytes := body.Bytes()
	frameLen := make([]byte, 4)
	binary.BigEndian.PutUint32(frameLen, uint32(len(bodyBytes)))
	frame.Write(frameLen)
	frame.Write([]byte{0, 0})
	frame.Write(bodyBytes)

	tagStart := 0
	var existingFrames []byte

	if len(data) >= 10 && string(data[:3]) == "ID3" {
		size := int(data[6]&0x7f)<<21 | int(data[7]&0x7f)<<14 |
			int(data[8]&0x7f)<<7 | int(data[9]&0x7f)
		tagStart = 10 + size
		if data[5]&0x10 != 0 {
			tagStart += 10
		}
		if tagStart > len(data) {
			tagStart = 10
		}
		existingFrames = data[10:tagStart-10]
	}

	var allFrames bytes.Buffer
	if len(existingFrames) > 0 {
		allFrames.Write(existingFrames)
	}
	allFrames.Write(frame.Bytes())

	totalSize := allFrames.Len()
	newTag := bytes.NewBuffer([]byte("ID3"))
	newTag.WriteByte(3)
	newTag.WriteByte(0)
	newTag.WriteByte(0)

	sizeBytes := make([]byte, 4)
	sizeBytes[0] = byte((totalSize >> 21) & 0x7f)
	sizeBytes[1] = byte((totalSize >> 14) & 0x7f)
	sizeBytes[2] = byte((totalSize >> 7) & 0x7f)
	sizeBytes[3] = byte(totalSize & 0x7f)
	newTag.Write(sizeBytes)
	newTag.Write(allFrames.Bytes())

	var output bytes.Buffer
	output.Write(newTag.Bytes())
	output.Write(data[tagStart:])

	if err := os.WriteFile(mp3Path, output.Bytes(), 0644); err != nil {
		return fmt.Errorf("write mp3: %w", err)
	}
	return nil
}

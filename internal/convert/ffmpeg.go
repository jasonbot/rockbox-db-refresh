package convert

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const DefaultSampleRate = 44100

func CheckFFmpeg() error {
	_, err := exec.LookPath("ffmpeg")
	return err
}

func EncodeMP3(ctx context.Context, input, output string, sampleRate int) error {
	args := []string{
		"-i", input,
		"-vn",
		"-q:a", "0",
		"-ar", strconv.Itoa(sampleRate),
		"-map_metadata", "0",
		"-map_metadata", "0:s:0",
		"-id3v2_version", "3",
		"-write_id3v1", "1",
		"-write_id3v2", "1",
		"-y", output,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &FFmpegError{Err: err, Stderr: stderr.String()}
	}
	return nil
}

type FFmpegError struct {
	Err    error
	Stderr string
}

func (e *FFmpegError) Error() string {
	return fmt.Sprintf("%v: %s", e.Err, e.Stderr)
}

func (e *FFmpegError) Unwrap() error {
	return e.Err
}

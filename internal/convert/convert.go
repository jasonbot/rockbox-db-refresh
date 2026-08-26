package convert

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"rbdb/internal/artwork"
	"rbdb/internal/meta"
	"rbdb/internal/musicbrainz"
	"rbdb/internal/walker"
)

type Options struct {
	SampleRate      int
	MaxArtDimension int
	MaxArtFileSize  int
	Workers         int
	MusicBrainz     *musicbrainz.Client
	EnrichArt       bool
	NormalizeMeta   bool
	NormalizeMode   string
	MinScore        int
}

type FileResult struct {
	InputPath  string
	OutputPath string
	Skipped    bool
	Err        error
}

func ConvertAll(ctx context.Context, inputDir, outputDir string, opts Options, onResult func(FileResult)) error {
	if opts.Workers <= 0 {
		opts.Workers = runtime.NumCPU()
	}
	if opts.SampleRate <= 0 {
		opts.SampleRate = DefaultSampleRate
	}
	if opts.MaxArtDimension <= 0 {
		opts.MaxArtDimension = artwork.DefaultMaxDim
	}
	if opts.MaxArtFileSize <= 0 {
		opts.MaxArtFileSize = artwork.DefaultMaxArtFileSize
	}
	if opts.MinScore <= 0 {
		opts.MinScore = 50
	}
	if opts.NormalizeMode == "" {
		opts.NormalizeMode = "fill"
	}

	dirs, err := walker.CollectDirs(inputDir)
	if err != nil {
		return err
	}
	dirs = append([]string{inputDir}, dirs...)

	var converted, skipped atomic.Int64
	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, opts.Workers)

	for _, dir := range dirs {
		files := walker.FindAudioFiles(dir)
		for _, inputPath := range files {
			inputPath := inputPath
			rel, _ := walker.RelativePath(inputDir, inputPath)
			outputPath := strings.TrimSuffix(filepath.Join(outputDir, rel), filepath.Ext(rel)) + ".mp3"

			if walker.OutputExists(outputPath) {
				skipped.Add(1)
				onResult(FileResult{InputPath: inputPath, OutputPath: outputPath, Skipped: true})
				continue
			}

			sem <- struct{}{}
			g.Go(func() error {
				defer func() { <-sem }()
				err := ConvertFile(ctx, inputPath, outputPath, opts)
				if err != nil {
					onResult(FileResult{InputPath: inputPath, OutputPath: outputPath, Err: err})
					return nil
				}
				converted.Add(1)
				onResult(FileResult{InputPath: inputPath, OutputPath: outputPath})
				return nil
			})
		}
	}

	_ = g.Wait()
	return nil
}

func ConvertFile(ctx context.Context, inputPath, outputPath string, opts Options) error {
	track, err := meta.ParseTrack(inputPath, "")
	if err != nil {
		return err
	}

	var match *musicbrainz.NormalizeResult
	if opts.MusicBrainz != nil && opts.NormalizeMeta {
		match, err = opts.MusicBrainz.NormalizeMetadata(ctx, track, musicbrainz.SearchOptions{MinScore: opts.MinScore})
		if err == nil && match != nil && match.HasMatch {
			switch opts.NormalizeMode {
			case "overwrite":
				track.Meta = match.Meta
				if match.Year > 0 {
					track.Year = match.Year
				}
				if match.TrackNumber > 0 {
					track.TrackNum = match.TrackNumber
				}
				if match.DiscNumber > 0 {
					track.Disc = match.DiscNumber
				}
			case "fill":
				if track.Meta.Title == "" {
					track.Meta.Title = match.Meta.Title
				}
				if track.Meta.Artist == "" {
					track.Meta.Artist = match.Meta.Artist
				}
				if track.Meta.Album == "" {
					track.Meta.Album = match.Meta.Album
				}
			}
		}
	}

	if opts.MusicBrainz != nil && opts.EnrichArt && track.CoverArt == nil {
		if match == nil || !match.HasMatch {
			match, err = opts.MusicBrainz.NormalizeMetadata(ctx, track, musicbrainz.SearchOptions{MinScore: opts.MinScore})
		}
		if err == nil && match != nil && match.HasMatch {
			raw, _, artErr := opts.MusicBrainz.FetchFrontCover(ctx, match.ReleaseID, match.ReleaseGroupID)
			if artErr == nil && raw != nil {
				processed, procErr := artwork.ProcessCoverArt(raw, opts.MaxArtDimension, opts.MaxArtFileSize)
				if procErr == nil {
					track.CoverArt = processed
					track.CoverArtMIME = "image/jpeg"
				}
			}
		}
	}

	tmpDir, err := os.MkdirTemp("", "rockbox_converter")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	noCoverPath := filepath.Join(tmpDir, "no_cover.mp3")
	if err := EncodeMP3(ctx, inputPath, noCoverPath, opts.SampleRate); err != nil {
		return err
	}

	if track.CoverArt != nil {
		processed, err := artwork.ProcessCoverArt(track.CoverArt, opts.MaxArtDimension, opts.MaxArtFileSize)
		if err == nil {
			if err := artwork.EmbedCoverArtToMP3(noCoverPath, processed, track.CoverArtMIME); err != nil {
				return os.Rename(noCoverPath, outputPath)
			}
		}
	}

	return os.Rename(noCoverPath, outputPath)
}

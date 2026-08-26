package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"

	"rbdb/internal/artwork"
	"rbdb/internal/meta"
	"rbdb/internal/musicbrainz"
	"rbdb/internal/progress"
	"rbdb/internal/tui"
	"rbdb/internal/walker"
)

var fixCommand = &cli.Command{
	Name:      "fix",
	Usage:     "Fix MP3 metadata and album art in-place",
	ArgsUsage: "<path>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "path",
			Usage:   "path to folder with MP3s",
			Aliases: []string{"p"},
		},
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "don't edit files",
			Aliases: []string{"n"},
		},
		&cli.StringFlag{
			Name:  "normalize",
			Usage: "metadata normalization mode: none, fill, overwrite",
			Value: "none",
		},
		&cli.BoolFlag{
			Name:  "no-art",
			Usage: "don't fetch album art",
		},
		&cli.BoolFlag{
			Name:    "no-tui",
			Usage:   "plain output instead of the interactive interface",
			Aliases: []string{"Q"},
		},
		&cli.IntFlag{
			Name:  "min-score",
			Usage: "minimum MusicBrainz search score (0-100)",
			Value: 50,
		},
		&cli.BoolFlag{
			Name:    "v",
			Usage:   "verbose output",
			Aliases: []string{"verbose"},
		},
	},
	Action: func(c *cli.Context) error {
		verbose = c.Bool("v")

		dir := c.String("path")
		if dir == "" && c.NArg() > 0 {
			dir = c.Args().First()
		}
		if dir == "" {
			return fmt.Errorf("usage: rbdb fix [options] <path>")
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("bad path: %w", err)
		}
		st, err := os.Stat(absDir)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("not a directory: %s", absDir)
		}

		normalize := c.String("normalize")
		if normalize != "none" && normalize != "fill" && normalize != "overwrite" {
			return fmt.Errorf("invalid normalize mode: %s (must be none, fill, or overwrite)", normalize)
		}

		dryRun := c.Bool("dry-run")
		noArt := c.Bool("no-art")
		noTUI := c.Bool("no-tui")
		minScore := c.Int("min-score")

		var mbClient *musicbrainz.Client
		if !noArt || normalize != "none" {
			mbClient, err = musicbrainz.NewClient(musicbrainz.DefaultUserAgent, "")
			if err != nil {
				return fmt.Errorf("failed to create MusicBrainz client: %w", err)
			}
		}

		opts := fixOptions{
			dir:         absDir,
			dryRun:      dryRun,
			normalize:   normalize,
			noArt:       noArt,
			minScore:    minScore,
			musicBrainz: mbClient,
		}

		useTUI := !noTUI && !dryRun && isTTY(os.Stdout)

		if useTUI {
			mp3Count := countMP3s(absDir)
			if mp3Count == 0 {
				fmt.Fprintf(os.Stderr, "no MP3 files found in %s\n", absDir)
				return nil
			}
			err = tui.RunFix(absDir, fixJob(opts))
			fmt.Println()
			if err != nil {
				return fmt.Errorf("interface error: %w", err)
			}
			return nil
		}

		ctx := context.Background()
		doneCh := make(chan error, 1)
		go fixJob(opts)(ctx, PlainProgressHandler("MP3", absDir, doneCh))
		WaitForDone(doneCh, absDir, "fix")
		return nil
	},
}

type fixOptions struct {
	dir         string
	dryRun      bool
	normalize   string
	noArt       bool
	minScore    int
	musicBrainz *musicbrainz.Client
}

func fixJob(opts fixOptions) func(ctx context.Context, send func(any)) {
	return func(ctx context.Context, send func(any)) {
		dirs, err := walker.CollectDirs(opts.dir)
		if err != nil {
			send(progress.Done{Err: err})
			return
		}
		dirs = append([]string{opts.dir}, dirs...)

		var mp3s []string
		for _, dir := range dirs {
			for _, f := range walker.FindAudioFiles(dir) {
				if strings.ToLower(filepath.Ext(f)) == ".mp3" {
					mp3s = append(mp3s, f)
				}
			}
		}

		send(progress.Found{N: len(mp3s)})
		if len(mp3s) == 0 {
			send(progress.Done{Err: fmt.Errorf("no MP3 files found in %s", opts.dir)})
			return
		}

		var started atomic.Int64
		g, ctx := errgroup.WithContext(ctx)
		sem := make(chan struct{}, runtime.NumCPU())

		for _, path := range mp3s {
			path := path
			sem <- struct{}{}
			g.Go(func() error {
				defer func() { <-sem }()

				n := started.Add(1)
				send(progress.FileStart{Path: path, Done: int(n), Total: len(mp3s)})

				if opts.dryRun {
					send(progress.FileDone{Path: path, Skipped: true})
					return nil
				}

				track, err := meta.ParseTrack(path, "")
				if err != nil {
					send(progress.FileDone{Path: path, Err: err})
					return nil
				}

				enrich, _ := musicbrainz.Enrich(ctx, opts.musicBrainz, track, path, musicbrainz.EnrichOptions{
					Mode:     opts.normalize,
					FetchArt: !opts.noArt && track.CoverArt == nil,
					MinScore: opts.minScore,
				}, send)

				if enrich == nil || !enrich.Normalized && !enrich.ArtworkFetched {
					send(progress.FileDone{Path: path, Skipped: true})
					return nil
				}

				if err := rewriteMP3(path, track); err != nil {
					send(progress.FileDone{Path: path, Err: err})
					return nil
				}

				send(progress.FileDone{Path: path})
				return nil
			})
		}

		_ = g.Wait()
		send(progress.Done{})
	}
}

func rewriteMP3(path string, track *meta.Track) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(path), ".rockbox_fix_*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, filepath.Base(path))

	args := []string{
		"-i", path,
		"-vn",
		"-c", "copy",
		"-map_metadata", "0",
		"-id3v2_version", "3",
	}

	if track.Meta.Title != "" {
		args = append(args, "-metadata", "title="+track.Meta.Title)
	}
	if track.Meta.Artist != "" {
		args = append(args, "-metadata", "artist="+track.Meta.Artist)
	}
	if track.Meta.Album != "" {
		args = append(args, "-metadata", "album="+track.Meta.Album)
	}
	if track.Meta.AlbumArtist != "" {
		args = append(args, "-metadata", "album_artist="+track.Meta.AlbumArtist)
	}
	if track.Meta.Genre != "" {
		args = append(args, "-metadata", "genre="+track.Meta.Genre)
	}

	args = append(args, "-y", tmpPath)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w", err)
	}

	if track.CoverArt != nil {
		if err := artwork.EmbedCoverArtToMP3(tmpPath, track.CoverArt, track.CoverArtMIME); err != nil {
			return err
		}
	}

	return os.Rename(tmpPath, path)
}

func countMP3s(dir string) int {
	dirs, _ := walker.CollectDirs(dir)
	dirs = append([]string{dir}, dirs...)
	n := 0
	for _, d := range dirs {
		for _, f := range walker.FindAudioFiles(d) {
			if strings.ToLower(filepath.Ext(f)) == ".mp3" {
				n++
			}
		}
	}
	return n
}

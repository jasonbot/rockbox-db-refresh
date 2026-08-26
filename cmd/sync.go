package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"

	"rbdb/internal/artwork"
	"rbdb/internal/convert"
	"rbdb/internal/meta"
	"rbdb/internal/musicbrainz"
	"rbdb/internal/progress"
	"rbdb/internal/tui"
	"rbdb/internal/walker"
)

var syncCommand = &cli.Command{
	Name:      "sync",
	Usage:     "Convert audio files from source to destination",
	ArgsUsage: "<origin> <destination>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "origin",
			Usage:   "location of original files",
			Aliases: []string{"o"},
		},
		&cli.StringFlag{
			Name:    "destination",
			Usage:   "relative root to output new files",
			Aliases: []string{"d"},
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
			Name:    "overwrite",
			Usage:   "overwrite existing files",
			Aliases: []string{"w"},
		},
		&cli.BoolFlag{
			Name:    "delete",
			Usage:   "remove files in destination that would not be created or overwritten",
			Aliases: []string{"x"},
		},
		&cli.BoolFlag{
			Name:  "update",
			Usage: "update rockbox database files with changes",
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

		origin := c.String("origin")
		destination := c.String("destination")

		if origin == "" && c.NArg() >= 2 {
			origin = c.Args().Get(0)
			destination = c.Args().Get(1)
		}

		if origin == "" || destination == "" {
			return fmt.Errorf("usage: rbdb sync [options] <origin> <destination>")
		}

		absOrigin, err := filepath.Abs(origin)
		if err != nil {
			return fmt.Errorf("bad origin path: %w", err)
		}
		st, err := os.Stat(absOrigin)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("not a directory: %s", absOrigin)
		}

		absDest, err := filepath.Abs(destination)
		if err != nil {
			return fmt.Errorf("bad destination path: %w", err)
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

		opts := syncOptions{
			origin:      absOrigin,
			destination: absDest,
			dryRun:      dryRun,
			normalize:   normalize,
			noArt:       noArt,
			overwrite:   c.Bool("overwrite"),
			deleteExtra: c.Bool("delete"),
			updateDB:    c.Bool("update"),
			minScore:    minScore,
			musicBrainz: mbClient,
		}

		useTUI := !noTUI && !dryRun && isTTY(os.Stdout)

		if useTUI {
			dirs, err := walker.CollectDirs(context.Background(), absOrigin)
			if err == nil {
				dirs = append([]string{absOrigin}, dirs...)
			}
			totalFiles := 0
			for _, d := range dirs {
				totalFiles += len(walker.FindAudioFiles(d))
			}
			if totalFiles == 0 {
				fmt.Fprintf(os.Stderr, "no audio files found in %s\n", absOrigin)
				return nil
			}
			err = tui.RunSync(absOrigin, absDest, syncJob(opts))
			fmt.Println()
			if err != nil {
				return fmt.Errorf("interface error: %w", err)
			}
			return nil
		}

		ctx := context.Background()
		doneCh := make(chan error, 1)
		go syncJob(opts)(ctx, PlainProgressHandler("audio", absOrigin, doneCh))
		WaitForDone(doneCh, absOrigin, "sync")
		return nil
	},
}

type syncOptions struct {
	origin      string
	destination string
	dryRun      bool
	normalize   string
	noArt       bool
	overwrite   bool
	deleteExtra bool
	updateDB    bool
	minScore    int
	musicBrainz *musicbrainz.Client
}

func syncJob(opts syncOptions) func(ctx context.Context, send func(any)) {
	return func(ctx context.Context, send func(any)) {
		send(progress.Banner{Lines: []string{
			fmt.Sprintf("origin:     %s", opts.origin),
			fmt.Sprintf("dest:       %s", opts.destination),
			fmt.Sprintf("normalize:  %s", opts.normalize),
			fmt.Sprintf("no-art:     %v", opts.noArt),
			fmt.Sprintf("overwrite:  %v", opts.overwrite),
			fmt.Sprintf("delete:     %v", opts.deleteExtra),
			fmt.Sprintf("dry-run:    %v", opts.dryRun),
			fmt.Sprintf("min-score:  %d", opts.minScore),
			fmt.Sprintf("workers:    %d", runtime.NumCPU()),
		}})

		dirs, err := walker.CollectDirs(ctx, opts.origin)
		if err != nil {
			send(progress.Done{Err: err})
			return
		}
		dirs = append([]string{opts.origin}, dirs...)

		var allFiles []string
		for _, dir := range dirs {
			files := walker.FindAudioFiles(dir)
			allFiles = append(allFiles, files...)
		}

		send(progress.Found{N: len(allFiles)})
		if len(allFiles) == 0 {
			send(progress.Done{Err: fmt.Errorf("no audio files found in %s", opts.origin)})
			return
		}

		type fileJob struct {
			inputPath  string
			outputPath string
		}
		jobs := make([]fileJob, 0, len(allFiles))
		outputFiles := make(map[string]bool, len(allFiles))
		for _, inputPath := range allFiles {
			rel, _ := walker.RelativePath(opts.origin, inputPath)
			outputPath := strings.TrimSuffix(filepath.Join(opts.destination, rel), filepath.Ext(rel)) + ".mp3"
			outputFiles[outputPath] = true
			jobs = append(jobs, fileJob{inputPath: inputPath, outputPath: outputPath})
		}

		var converted, skipped, failed atomic.Int64
		g, ctx := errgroup.WithContext(ctx)
		sem := make(chan struct{}, runtime.NumCPU())

		for _, job := range jobs {
			job := job
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			g.Go(func() error {
				defer func() { <-sem }()

				if ctx.Err() != nil {
					return ctx.Err()
				}

				send(progress.FileStart{Path: job.inputPath, Done: int(converted.Load()+skipped.Load()+failed.Load()+1), Total: len(allFiles)})

				if !opts.overwrite && walker.OutputExists(job.outputPath) {
					skipped.Add(1)
					send(progress.FileDone{Path: job.inputPath, Skipped: true})
					return nil
				}

				if opts.dryRun {
					skipped.Add(1)
					send(progress.FileDone{Path: job.inputPath, Skipped: true})
					return nil
				}

				track, err := meta.ParseTrack(job.inputPath, "")
				if err != nil {
					failed.Add(1)
					send(progress.FileDone{Path: job.inputPath, Err: err})
					return nil
				}

				if ctx.Err() != nil {
					return ctx.Err()
				}

				musicbrainz.Enrich(ctx, opts.musicBrainz, track, job.inputPath, musicbrainz.EnrichOptions{
					Mode:     opts.normalize,
					FetchArt: !opts.noArt && track.CoverArt == nil,
					MinScore: opts.minScore,
				}, send)

				if ctx.Err() != nil {
					return ctx.Err()
				}

				if err := convert.ConvertFile(ctx, job.inputPath, job.outputPath, convert.Options{
					SampleRate:      convert.DefaultSampleRate,
					MaxArtDimension: artwork.DefaultMaxDim,
					MaxArtFileSize:  artwork.DefaultMaxArtFileSize,
				}); err != nil {
					failed.Add(1)
					send(progress.FileDone{Path: job.inputPath, Err: err})
					return nil
				}

				if track.CoverArt != nil {
					if err := artwork.EmbedCoverArtToMP3(job.outputPath, track.CoverArt, track.CoverArtMIME); err != nil {
						vlogf("Warning: failed to embed art for %s: %v", filepath.Base(job.inputPath), err)
					}
				}

				converted.Add(1)
				send(progress.FileDone{Path: job.inputPath})
				return nil
			})
		}

		_ = g.Wait()

		if opts.deleteExtra && !opts.dryRun {
			deleteExtraFiles(opts.destination, outputFiles)
		}

		if opts.updateDB && !opts.dryRun {
			rbDir := findRockboxRoot(opts.destination)
			if rbDir != "" {
				vlogf("Updating rockbox database in %s", rbDir)
			}
		}

		send(progress.Done{})
	}
}

func deleteExtraFiles(destDir string, keepFiles map[string]bool) {
	filepath.Walk(destDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !keepFiles[path] {
			vlogf("Deleting extra file: %s", path)
			os.Remove(path)
		}
		return nil
	})
}

func findRockboxRoot(dir string) string {
	parent := dir
	for i := 0; i < 3; i++ {
		rbDir := filepath.Join(parent, ".rockbox")
		if st, err := os.Stat(rbDir); err == nil && st.IsDir() {
			return parent
		}
		parent = filepath.Dir(parent)
	}
	return ""
}

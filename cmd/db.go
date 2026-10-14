package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"rbdb/internal/config"
	"rbdb/internal/db"
	"rbdb/internal/meta"
	"rbdb/internal/progress"
	"rbdb/internal/shuffle"
	"rbdb/internal/tui"
)

var verbose bool

func vlogf(format string, args ...any) {
	if verbose {
		fmt.Printf(format+"\n", args...)
	}
}

type dbOptions struct {
	dryRun         bool
	refresh        bool
	shuffle        bool
	shuffleLimit   int
	shuffleRecency float64
}

var dbCommand = &cli.Command{
	Name:      "db",
	Usage:     "Build/update Rockbox tagcache database",
	ArgsUsage: "[path]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "root",
			Usage:   "device root directory (mount point)",
			Aliases: []string{"r"},
		},
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "scan and parse only, do not write",
			Aliases: []string{"n"},
		},
		&cli.BoolFlag{
			Name:  "refresh",
			Usage: "incremental update: reuse metadata for unchanged files (matched by path+mtime), drop deleted files, and preserve play counts/ratings",
		},
		&cli.BoolFlag{
			Name:    "shuffle",
			Usage:   "after building, create a new shuffled playlist in the device's playlist catalogue (shown in the player's Playlists menu)",
			Aliases: []string{"s"},
		},
		&cli.IntFlag{
			Name:  "shuffle-limit",
			Usage: "max tracks in the shuffled playlist (firmware default max is 10000)",
			Value: shuffle.ShuffleLimitDefault,
		},
		&cli.Float64Flag{
			Name:  "shuffle-recency",
			Usage: "recency bias strength for -shuffle: newer files tend to sit nearer the front; 0 = uniform shuffle",
			Value: 2.0,
		},
		&cli.IntFlag{
			Name:  "max-tag",
			Usage: "truncate non-path metadata strings to this many bytes (rune-safe); paths are kept up to 1024 bytes",
			Value: 512,
		},
		&cli.BoolFlag{
			Name:    "no-tui",
			Usage:   "plain output instead of the interactive interface",
			Aliases: []string{"Q"},
		},
		&cli.BoolFlag{
			Name:    "v",
			Usage:   "verbose output",
			Aliases: []string{"verbose"},
		},
	},
	Action: func(c *cli.Context) error {
		verbose = c.Bool("v")
		db.MaxStringTagLen = c.Int("max-tag")

		rootPath := c.String("root")
		explicit := rootPath != ""
		if !explicit && c.NArg() > 0 {
			rootPath = c.Args().First()
			explicit = true
		}

		dryRun := c.Bool("dry-run")
		noTUI := c.Bool("no-tui")
		useTUI := !noTUI && !dryRun && isTTY(os.Stdout)

		if !explicit {
			cached := config.ReadLastRoot()
			switch {
			case useTUI:
				picked, perr := tui.PickRoot(cached)
				if perr != nil {
					return fmt.Errorf("interface error: %w", perr)
				}
				if picked == "" {
					return nil
				}
				rootPath = picked
			case cached != "":
				rootPath = cached
				fmt.Printf("using last root %s (pass a path or -root to override)\n", cached)
			default:
				rootPath = "."
			}
		}

		absRoot, err := filepath.Abs(rootPath)
		if err != nil {
			return fmt.Errorf("bad root: %w", err)
		}
		st, err := os.Stat(absRoot)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("not a directory: %s", absRoot)
		}
		config.SaveLastRoot(absRoot)

		opts := dbOptions{
			dryRun:         dryRun,
			refresh:        c.Bool("refresh"),
			shuffle:        c.Bool("shuffle"),
			shuffleLimit:   c.Int("shuffle-limit"),
			shuffleRecency: c.Float64("shuffle-recency"),
		}

		if useTUI {
			err = tui.Run(absRoot, dbJob(absRoot, opts), opts.refresh)
			fmt.Println()
			if err != nil {
				return fmt.Errorf("interface error: %w", err)
			}
			return nil
		}

		ctx := context.Background()
		lastParse := 0
		shuffleCount := 0
		shufflePath := ""
		doneCh := make(chan error, 1)
		go dbJob(absRoot, opts)(ctx, func(m any) {
			switch msg := m.(type) {
			case progress.Found:
				fmt.Printf("Found %d candidate audio files under %s\n", msg.N, absRoot)
			case progress.Parse:
				if msg.Done-lastParse >= 500 || msg.Done == msg.Total {
					fmt.Printf("\rParsed %d/%d files...", msg.Done, msg.Total)
					lastParse = msg.Done
				}
			case progress.Skip:
				vlogf("SKIP %s (%v)", msg.Path, msg.Err)
			case progress.Refresh:
				fmt.Printf("refresh: kept %d, updated %d, added %d, removed %d\n",
					msg.Kept, msg.Updated, msg.Added, msg.Removed)
			case progress.TagStart:
				name, _ := tui.TagNames[msg.Tag]
				vlogf("writing %s (%s)", tui.FileName(msg.Tag), name)
			case progress.Shuffle:
				if msg.Err != nil {
					fmt.Printf("\rshuffle playlist failed: %v\n", msg.Err)
				} else {
					vlogf("shuffled playlist: %d tracks -> %s", msg.N, msg.Path)
					shuffleCount = msg.N
					shufflePath = msg.Path
				}
			case progress.Done:
				doneCh <- msg.Err
			}
		})
		if err := <-doneCh; err != nil {
			fmt.Println()
			return fmt.Errorf("build failed: %w", err)
		}
		if !dryRun {
			fmt.Printf("\rWrote tagcache database to %s/.rockbox            \n", absRoot)
			if shuffleCount > 0 {
				fmt.Printf("Created shuffled playlist (%d tracks): %s\n", shuffleCount, shufflePath)
				fmt.Println("It now appears in the player's Playlists menu.")
			}
		} else {
			fmt.Println("\nDry run complete.")
		}
		return nil
	},
}

func dbJob(root string, opt dbOptions) func(ctx context.Context, send func(any)) {
	return func(ctx context.Context, send func(any)) {
		err := runDB(ctx, root, opt, send)
		if errors.Is(err, context.Canceled) {
			send(progress.Cancelled{})
			return
		}
		send(progress.Done{Err: err})
	}
}

// runDB performs a tagcache build or refresh, emitting Found/Parse progress
// plus TagStart/TagDone, Refresh and Shuffle messages. It deliberately does
// not emit Done or Cancelled: callers such as sync run it as a sub-step and
// decide when the overall job is finished. A context cancellation is returned
// as context.Canceled.
func runDB(ctx context.Context, root string, opt dbOptions, send func(any)) error {
	rbDir := filepath.Join(root, ".rockbox")
	var files []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			vlogf("walk error: %s: %v", path, err)
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".rockbox" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if meta.AudioExts[strings.ToLower(filepath.Ext(name))] {
			files = append(files, path)
		}
		return nil
	})
	send(progress.Found{N: len(files)})
	if len(files) == 0 {
		return fmt.Errorf("no supported audio files found under %s", root)
	}

	oldByPath := map[string]*meta.Track{}
	refreshing := false
	if opt.refresh {
		old, err := db.ReadDatabase(rbDir)
		if err != nil {
			vlogf("refresh: no usable existing database (%v); full rebuild", err)
		} else {
			for _, t := range old {
				oldByPath[t.DevPath] = t
			}
			refreshing = true
			vlogf("refresh: %d entries in existing database", len(old))
		}
	}

	tracks := make([]*meta.Track, 0, len(files))
	reused, updated, added := 0, 0, 0
	for _, host := range files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, err := filepath.Rel(root, host)
		if err != nil {
			continue
		}
		devPath := "/" + filepath.ToSlash(rel)

		if refreshing {
			if ot, ok := oldByPath[devPath]; ok {
				delete(oldByPath, devPath)
				if st, serr := os.Stat(host); serr == nil && st.ModTime().Unix() == ot.MTime {
					tracks = append(tracks, ot)
					reused++
					send(progress.Parse{Done: len(tracks), Total: len(files), Path: devPath, Reused: true})
					continue
				}
				updated++
			} else {
				added++
			}
		}

		t, perr := parseTrackSafe(host, devPath)
		if perr != nil {
			tracks = append(tracks, nil)
			send(progress.Skip{Path: devPath, Err: perr})
			send(progress.Parse{Done: len(tracks), Total: len(files), Path: devPath})
			continue
		}
		tracks = append(tracks, t)
		send(progress.Parse{Done: len(tracks), Total: len(files), Path: devPath})
	}
	removed := len(oldByPath)

	if opt.dryRun {
		if refreshing {
			send(progress.Refresh{Kept: reused, Updated: updated, Added: added, Removed: removed})
		}
		return nil
	}

	if err := os.MkdirAll(rbDir, 0755); err != nil {
		return err
	}
	os.Remove(filepath.Join(rbDir, "database_tmp.tcd"))

	kept := tracks[:0]
	for _, t := range tracks {
		if t != nil {
			kept = append(kept, t)
		}
	}

	err := db.Build(rbDir, kept, func(tag int, done bool) {
		if done {
			send(progress.TagDone{Tag: tag})
		} else {
			send(progress.TagStart{Tag: tag})
		}
	})
	if err != nil {
		return err
	}

	if refreshing {
		send(progress.Refresh{Kept: reused, Updated: updated, Added: added, Removed: removed})
	}

	if opt.shuffle {
		addedDates := shuffle.ApplyAddedDates(rbDir, kept)
		if serr := shuffle.WriteAddedDates(rbDir, addedDates); serr != nil {
			vlogf("could not save add dates: %v", serr)
		}
		catDir := shuffle.CatalogDir(root, rbDir)
		plName, n, serr := shuffle.WriteCatalogPlaylist(catDir, kept, opt.shuffleLimit, opt.shuffleRecency, time.Now())
		plPath := ""
		if serr == nil {
			plPath = filepath.Join(catDir, plName)
		}
		send(progress.Shuffle{N: n, Path: plPath, Err: serr})
	}

	return nil
}

func parseTrackSafe(host, devPath string) (t *meta.Track, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, err = nil, fmt.Errorf("malformed file: %v", r)
		}
	}()
	return meta.ParseTrack(host, devPath)
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// jobOptions carries the flags that steer the build pipeline.
type jobOptions struct {
	dryRun         bool
	refresh        bool
	shuffle        bool
	shuffleLimit   int
	shuffleRecency float64
}

// job is the actual build pipeline. It reports progress through send and
// stops early when ctx is cancelled (between files).
func job(root string, opt jobOptions) func(ctx context.Context, send func(any)) {
	return func(ctx context.Context, send func(any)) {
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
			send(progress.Done{Err: fmt.Errorf("no supported audio files found under %s", root)})
			return
		}

		// -refresh: load the previous database so unchanged files (matched
		// by device path and mtime) skip parsing and keep their play
		// counts/ratings; paths absent from the scan are dropped.
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
				send(progress.Cancelled{})
				return
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
			send(progress.Done{})
			return
		}

		if err := os.MkdirAll(rbDir, 0755); err != nil {
			send(progress.Done{Err: err})
			return
		}
		os.Remove(filepath.Join(rbDir, "database_tmp.tcd"))

		// Drop placeholder entries for skipped files.
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
			send(progress.Done{Err: err})
			return
		}

		if refreshing {
			send(progress.Refresh{Kept: reused, Updated: updated, Added: added, Removed: removed})
		}

		if opt.shuffle {
			// Stamp stable add dates (seeded from ctime on first sight)
			// before shuffling, then persist them for future refreshes.
			addedDates := shuffle.ApplyAddedDates(rbDir, kept)
			if serr := shuffle.WriteAddedDates(rbDir, addedDates); serr != nil {
				vlogf("could not save add dates: %v", serr)
			}
			n, serr := shuffle.WriteShuffledPlaylist("/"+filepath.Base(rbDir), rbDir, kept, opt.shuffleLimit, opt.shuffleRecency)
			send(progress.Shuffle{N: n, Err: serr})
		}

		send(progress.Done{})
	}
}

func parseTrackSafe(host, devPath string) (t *meta.Track, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, err = nil, fmt.Errorf("malformed file: %v", r)
		}
	}()
	return meta.ParseTrack(host, devPath)
}

func main() {
	root := flag.String("root", "", "device root directory (mount point)")
	dry := flag.Bool("dry-run", false, "scan and parse only, do not write")
	refresh := flag.Bool("refresh", false,
		"incremental update: reuse metadata for unchanged files (matched by path+mtime), drop deleted files, and preserve play counts/ratings")
	shuffleFlag := flag.Bool("shuffle", false, "after building, install a shuffled playlist of the library as the player's current dynamic playlist (.rockbox/dynamic.m3u8 + .playlist_control)")
	shuffleLimit := flag.Int("shuffle-limit", shuffle.ShuffleLimitDefault,
		"max tracks in the shuffled playlist (firmware default max is 10000)")
	shuffleRecency := flag.Float64("shuffle-recency", 2.0,
		"recency bias strength for -shuffle: newer files tend to sit nearer the front; 0 = uniform shuffle")
	flag.IntVar(&db.MaxStringTagLen, "max-tag", 512,
		"truncate non-path metadata strings to this many bytes (rune-safe); paths are kept up to 1024 bytes")
	noTUI := flag.Bool("no-tui", false, "plain output instead of the interactive interface")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.Parse()

	rootPath := *root
	explicit := rootPath != ""
	if !explicit && flag.NArg() > 0 {
		rootPath = flag.Arg(0)
		explicit = true
	}

	useTUI := !*noTUI && !*dry && isTTY(os.Stdout)

	if !explicit {
		cached := config.ReadLastRoot()
		switch {
		case useTUI:
			// Let the user confirm/change the root interactively, starting
			// from the last used location; never auto-start the build.
			picked, perr := tui.PickRoot(cached)
			if perr != nil {
				fatalf("interface error: %v", perr)
			}
			if picked == "" {
				return
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
		fatalf("bad root: %v", err)
	}
	st, err := os.Stat(absRoot)
	if err != nil || !st.IsDir() {
		fatalf("not a directory: %s", absRoot)
	}
	config.SaveLastRoot(absRoot)

	opts := jobOptions{
		refresh:        *refresh,
		shuffle:        *shuffleFlag,
		shuffleLimit:   *shuffleLimit,
		shuffleRecency: *shuffleRecency,
	}

	if useTUI {
		err = tui.Run(absRoot, job(absRoot, opts), *refresh)
		fmt.Println()
		if err != nil {
			fatalf("interface error: %v", err)
		}
		return
	}
	ctx := context.Background()
	lastParse := 0
	shuffleCount := 0
	doneCh := make(chan error, 1)
	opts.dryRun = *dry
	go job(absRoot, opts)(ctx, func(m any) {
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
				vlogf("shuffled playlist: %d tracks -> /%s", msg.N, shuffle.PlaylistFile)
				shuffleCount = msg.N
			}
		case progress.Done:
			doneCh <- msg.Err
		}
	})
	if err := <-doneCh; err != nil {
		fmt.Println()
		fatalf("build failed: %v", err)
	}
	if !*dry {
		fmt.Printf("\rWrote tagcache database to %s/.rockbox            \n", absRoot)
		if shuffleCount > 0 {
			fmt.Printf("Installed shuffled playlist (%d tracks): %s/.rockbox/%s\n", shuffleCount, absRoot, shuffle.PlaylistFile)
			fmt.Println("It becomes the player's current playlist on next boot; playback resume still depends on your device's autoresume/bookmark settings.")
		}
	} else {
		fmt.Println("\nDry run complete.")
	}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

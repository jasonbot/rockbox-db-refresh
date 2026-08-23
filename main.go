package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var verbose bool

func vlogf(format string, args ...any) {
	if verbose {
		fmt.Printf(format+"\n", args...)
	}
}

// job is the actual build pipeline. It reports progress through send and
// stops early when ctx is cancelled (between files).
func job(root string, dryRun, shuffle bool, shuffleLimit int, shuffleRecency float64) func(ctx context.Context, send func(any)) {
	return func(ctx context.Context, send func(any)) {
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
			if audioExts[strings.ToLower(filepath.Ext(name))] {
				files = append(files, path)
			}
			return nil
		})
		send(msgFound{len(files)})
		if len(files) == 0 {
			send(msgDone{fmt.Errorf("no supported audio files found under %s", root)})
			return
		}

		tracks := make([]*Track, 0, len(files))
		for _, host := range files {
			select {
			case <-ctx.Done():
				send(msgCancelled{})
				return
			default:
			}
			rel, err := filepath.Rel(root, host)
			if err != nil {
				continue
			}
			devPath := "/" + filepath.ToSlash(rel)

			t, perr := parseTrackSafe(host, devPath)
			if perr != nil {
				tracks = append(tracks, nil)
				send(msgSkip{path: devPath, err: perr})
				send(msgParse{done: len(tracks), total: len(files), path: devPath})
				continue
			}
			tracks = append(tracks, t)
			send(msgParse{done: len(tracks), total: len(files), path: devPath})
		}

		if dryRun {
			send(msgDone{nil})
			return
		}

		rbDir := filepath.Join(root, ".rockbox")
		if err := os.MkdirAll(rbDir, 0755); err != nil {
			send(msgDone{err})
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

		err := Build(rbDir, kept, func(tag int, done bool) {
			if done {
				send(msgTagDone{tag})
			} else {
				send(msgTagStart{tag})
			}
		})
		if err != nil {
			send(msgDone{err})
			return
		}

		if shuffle {
			n, serr := writeShuffledPlaylist("/"+filepath.Base(rbDir), rbDir, kept, shuffleLimit, shuffleRecency)
			send(msgShuffle{n: n, err: serr})
		}

		send(msgDone{nil})
	}
}

func parseTrackSafe(host, devPath string) (t *Track, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, err = nil, fmt.Errorf("malformed file: %v", r)
		}
	}()
	return parseTrack(host, devPath)
}

func main() {
	root := flag.String("root", "", "device root directory (mount point)")
	dry := flag.Bool("dry-run", false, "scan and parse only, do not write")
	shuffle := flag.Bool("shuffle", false, "after building, install a shuffled playlist of the library as the player's current dynamic playlist (.rockbox/dynamic.m3u8 + .playlist_control)")
	shuffleLimit := flag.Int("shuffle-limit", shuffleLimitDefault,
		"max tracks in the shuffled playlist (firmware default max is 10000)")
	shuffleRecency := flag.Float64("shuffle-recency", 2.0,
		"recency bias strength for -shuffle: newer files (by ctime) tend to sit nearer the front; 0 = uniform shuffle")
	flag.IntVar(&maxStringTagLen, "max-tag", 512,
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
		cached := readLastRoot()
		switch {
		case useTUI:
			// Let the user confirm/change the root interactively, starting
			// from the last used location; never auto-start the build.
			picked, perr := pickRoot(cached)
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
	saveLastRoot(absRoot)

	if useTUI {
		err = runTUI(absRoot, job(absRoot, false, *shuffle, *shuffleLimit, *shuffleRecency))
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
	go job(absRoot, *dry, *shuffle, *shuffleLimit, *shuffleRecency)(ctx, func(m any) {
		switch msg := m.(type) {
		case msgFound:
			fmt.Printf("Found %d candidate audio files under %s\n", msg.n, absRoot)
		case msgParse:
			if msg.done-lastParse >= 500 || msg.done == msg.total {
				fmt.Printf("\rParsed %d/%d files...", msg.done, msg.total)
				lastParse = msg.done
			}
		case msgSkip:
			vlogf("SKIP %s (%v)", msg.path, msg.err)
		case msgTagStart:
			name, _ := tagNames[msg.tag]
			vlogf("writing %s (%s)", fileName(msg.tag), name)
		case msgShuffle:
			if msg.err != nil {
				fmt.Printf("\rshuffle playlist failed: %v\n", msg.err)
			} else {
				vlogf("shuffled playlist: %d tracks -> %s", msg.n, "/"+shufflePlaylistFile)
				shuffleCount = msg.n
			}
		case msgDone:
			doneCh <- msg.err
		}
	})
	if err := <-doneCh; err != nil {
		fmt.Println()
		fatalf("build failed: %v", err)
	}
	if !*dry {
		fmt.Printf("\rWrote tagcache database to %s/.rockbox            \n", absRoot)
		if shuffleCount > 0 {
			fmt.Printf("Installed shuffled playlist (%d tracks): %s/.rockbox/%s\n", shuffleCount, absRoot, shufflePlaylistFile)
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

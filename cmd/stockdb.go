package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v2"

	"rbdb/internal/artwork"
	"rbdb/internal/config"
	"rbdb/internal/db"
	"rbdb/internal/ipod"
	"rbdb/internal/meta"
	"rbdb/internal/progress"
	"rbdb/internal/shuffle"
	"rbdb/internal/tui"
)

// stockMaxBytesDefault keeps the generated iTunesDB under the 7G Classic's
// boot-time memory budget: 56.0 MiB was observed to boot and 56.6 MiB to
// hang, so the default sits ~1.2 MB below the known-good point.
const stockMaxBytesDefault = 58000000

type stockOptions struct {
	root           string
	dryRun         bool
	classic        bool
	firewire       []byte
	fireWireStr    string
	model          string
	maxBytes       int
	recency        float64
	noRotate       bool
	artwork        bool
	shuffle        bool
	shuffleLimit   int
	shuffleRecency float64
}

var stockCommand = &cli.Command{
	Name:      "sync-stock-db",
	Usage:     "Write an iPod stock iTunesDB from the Rockbox tagcache database",
	ArgsUsage: "[path]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "root",
			Usage:   "device root directory (mount point)",
			Aliases: []string{"r"},
		},
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "parse and generate in memory only, do not write files",
			Aliases: []string{"n"},
		},
		&cli.StringFlag{
			Name:  "firewire-guid",
			Usage: "device FireWire GUID (hex) used for the 6G/7G HASH58 checksum; auto-read from iPod_Control/Device/SysInfo if omitted",
		},
		&cli.BoolFlag{
			Name:  "classic",
			Usage: "force 6G/7G Classic format regardless of the SysInfo model number",
		},
		&cli.IntFlag{
			Name:  "stock-max-bytes",
			Usage: "maximum iTunesDB size in bytes; when the library would exceed it, whole albums are rotated out (recency-weighted) until it fits",
			Value: stockMaxBytesDefault,
		},
		&cli.Float64Flag{
			Name:  "stock-recency",
			Usage: "recency bias strength for album rotation: newer albums are more likely to be kept; 0 = uniform random",
			Value: 2.0,
		},
		&cli.BoolFlag{
			Name:  "no-stock-rotate",
			Usage: "fail instead of rotating albums out when the iTunesDB would exceed -stock-max-bytes",
		},
		&cli.BoolFlag{
			Name:  "artwork",
			Usage: "extract embedded album art and write ArtworkDB + ithmb files for Cover Flow (Classic only)",
		},
		&cli.BoolFlag{
			Name:    "shuffle",
			Usage:   "add a shuffled \"New Shuffle\" playlist to the stock database (shown in the stock firmware's Playlists menu); rebuilt on every sync",
			Aliases: []string{"s"},
		},
		&cli.IntFlag{
			Name:  "shuffle-limit",
			Usage: "max tracks in the \"New Shuffle\" playlist",
			Value: shuffle.ShuffleLimitDefault,
		},
		&cli.Float64Flag{
			Name:  "shuffle-recency",
			Usage: "recency bias strength for -shuffle: newer files tend to sit nearer the front; 0 = uniform shuffle",
			Value: 2.0,
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

		// Read the Rockbox database.
		rbDir := filepath.Join(absRoot, ".rockbox")
		if _, err := os.Stat(filepath.Join(rbDir, "database_idx.tcd")); err != nil {
			return fmt.Errorf("no Rockbox tagcache database at %s (run 'rbdb db' first)", rbDir)
		}
		tracks, err := db.ReadDatabase(rbDir)
		if err != nil {
			return fmt.Errorf("reading Rockbox database: %w", err)
		}
		if len(tracks) == 0 {
			return fmt.Errorf("Rockbox database is empty")
		}

		// Device type is detected from the model number in SysInfo; this also
		// yields the FireWire GUID for the 6G/7G HASH58 checksum. The -classic
		// flag overrides detection and forces the 6G/7G format.
		model, fwStr := readSysInfo(absRoot)
		classic := ipod.IsClassic(model)
		if c.Bool("classic") {
			classic = true
		}

		// FireWire GUID: flag wins, else the auto-read device SysInfo value.
		if s := c.String("firewire-guid"); s != "" {
			fwStr = s
		}
		var fw []byte
		if classic && fwStr != "" {
			fw, err = ipod.DecodeGUID(fwStr)
			if err != nil {
				return err
			}
		}

		opts := stockOptions{
			root:           absRoot,
			dryRun:         dryRun,
			classic:        classic,
			firewire:       fw,
			fireWireStr:    fwStr,
			model:          model,
			maxBytes:       c.Int("stock-max-bytes"),
			recency:        c.Float64("stock-recency"),
			noRotate:       c.Bool("no-stock-rotate"),
			artwork:        c.Bool("artwork"),
			shuffle:        c.Bool("shuffle"),
			shuffleLimit:   c.Int("shuffle-limit"),
			shuffleRecency: c.Float64("shuffle-recency"),
		}

		if useTUI {
			err = tui.RunStock(absRoot, stockJob(opts), classic)
			fmt.Println()
			if err != nil {
				return fmt.Errorf("interface error: %w", err)
			}
			return nil
		}

		done := make(chan error, 1)
		shuffleN := 0
		stockJob(opts)(context.Background(), func(m any) {
			switch msg := m.(type) {
			case progress.Found:
				fmt.Fprintf(os.Stderr, "read %d tracks from the Rockbox database\n", msg.N)
			case progress.StockDB:
				if msg.Err != nil {
					done <- msg.Err
					return
				}
				rotated := ""
				if msg.Dropped > 0 {
					rotated = fmt.Sprintf(", %d tracks rotated out to fit the %d-byte budget", msg.Dropped, opts.maxBytes)
				}
			art := ""
			if msg.ArtImages > 0 || msg.ArtAlbums > 0 {
				art = fmt.Sprintf(", artwork: %d images (%d albums with art, %d failed)", msg.ArtImages, msg.ArtAlbums, msg.ArtFailed)
			}
			shuffleN = msg.ShuffleN
			shuf := ""
			if msg.ShuffleN > 0 {
				shuf = fmt.Sprintf(", %q playlist: %d tracks", shuffle.StockPlaylistName, msg.ShuffleN)
			}
			if opts.classic {
				fmt.Fprintf(os.Stderr, "%d tracks, %d albums%s%s%s -> %s format (HASH58)\n", msg.N, msg.Albums, rotated, art, shuf, classicLabel(msg.Classic))
			} else {
				fmt.Fprintf(os.Stderr, "%d tracks, %d albums%s%s%s -> %s format\n", msg.N, msg.Albums, rotated, art, shuf, classicLabel(msg.Classic))
			}
				if msg.ArtErr != nil {
					fmt.Fprintf(os.Stderr, "artwork failed: %v\n", msg.ArtErr)
				}
				done <- nil
			case progress.Done:
				done <- msg.Err
			}
		})
		if err := <-done; err != nil {
			fmt.Println()
			return fmt.Errorf("stock db failed: %w", err)
		}
		dest := filepath.Join(absRoot, filepath.Join(ipod.InstallPath(), ipod.DatabaseFile))
		if dryRun {
			fmt.Printf("\rDry run: would write %s            \n", dest)
		} else {
			fmt.Printf("\rWrote stock database to %s            \n", dest)
			if shuffleN > 0 {
				fmt.Printf("The stock firmware's Playlists menu now has a %q playlist (%d tracks).\n", shuffle.StockPlaylistName, shuffleN)
			}
		}
		return nil
	},
}

func classicLabel(classic bool) string {
	if classic {
		return "6G/7G Classic"
	}
	return "5G Video"
}

func stockJob(opts stockOptions) func(ctx context.Context, send func(any)) {
	return func(ctx context.Context, send func(any)) {
		format := "6G/7G Classic"
		if !opts.classic {
			format = "5G/5.5G Video"
		}
		send(progress.Banner{Lines: []string{
			"root:       " + opts.root,
			"model:      " + nonEmptyOr(opts.model, "(unknown, assumed 5G/5.5G Video)"),
			"format:     " + format,
			"dry-run:    " + fmt.Sprint(opts.dryRun),
			"firewire:   " + guidOrNone(opts.fireWireStr),
		}})

		rbDir := filepath.Join(opts.root, ".rockbox")
		tracks, err := db.ReadDatabase(rbDir)
		if err != nil {
			send(progress.StockDB{Err: fmt.Errorf("reading Rockbox database: %w", err)})
			return
		}
		send(progress.Found{N: len(tracks)})

		if opts.classic && len(opts.firewire) == 0 {
			send(progress.StockDB{Err: fmt.Errorf("6G/7G format needs the device FireWire GUID; set -firewire-guid or provide iPod_Control/Device/SysInfo")})
			return
		}

		// Stamp stable add dates (the sidecar is shared with db --shuffle)
		// so the rotation lottery treats tracks consistently across syncs.
		added := shuffle.ApplyAddedDates(rbDir, tracks)
		if !opts.dryRun {
			// Best effort: without it, unseen paths just reseed from ctime.
			_ = shuffle.WriteAddedDates(rbDir, added)
		}

		ipOpts := ipod.Options{Classic: opts.classic, FireWire: opts.firewire}

		// With -shuffle, reserve the exact byte cost of the playlist up front
		// so the rotation budget accounts for it; the playlist members are
		// picked from the surviving tracks after the rotation below.
		maxBytes := opts.maxBytes
		if opts.shuffle {
			plMax := min(opts.shuffleLimit, len(tracks))
			maxBytes -= ipod.PlaylistReserve(opts.classic, shuffle.StockPlaylistName, plMax)
			if maxBytes <= 0 {
				send(progress.StockDB{Err: fmt.Errorf("the %q playlist (%d tracks) alone exceeds the %d-byte budget", shuffle.StockPlaylistName, plMax, opts.maxBytes)})
				return
			}
		}

		data, lay, err := ipod.BuildBytes(tracks, ipOpts)
		dropped := 0
		kept := tracks
		if err == nil && len(data) > maxBytes {
			if opts.noRotate {
				err = fmt.Errorf("iTunesDB is %d bytes, over the %d-byte budget (rotation disabled with -no-stock-rotate)", len(data)+opts.maxBytes-maxBytes, opts.maxBytes)
			} else {
				// Over budget: rotate whole albums out, newest-kept bias.
				ordered := shuffle.RecencyAlbumOrder(tracks, opts.recency, ipod.AlbumKey)
				data, lay, dropped, err = ipod.FitBytes(ordered, ipOpts, maxBytes)
				if err == nil {
					kept = ordered[:lay.Tracks]
				}
			}
		}
		if err != nil {
			send(progress.StockDB{Err: err})
			return
		}

		// The "New Shuffle" stock-firmware playlist: a recency-weighted pick
		// of the tracks that actually made the database (rotated-out tracks
		// have no mhit to reference). The database is rebuilt from scratch on
		// every sync, so the single playlist of this name is overwritten each
		// run — the same shuffle as db --shuffle, inside the iTunesDB.
		shuffleN := 0
		if opts.shuffle {
			picked := shuffle.PickShuffled(kept, opts.shuffleLimit, opts.shuffleRecency)
			if len(picked) > 0 {
				ipOpts.Playlists = []ipod.Playlist{{Name: shuffle.StockPlaylistName, Tracks: picked}}
				data, lay, err = ipod.BuildBytes(kept, ipOpts)
				if err != nil {
					send(progress.StockDB{Err: err})
					return
				}
				shuffleN = len(picked)
			}
		}

		artImages, artAlbums, artFailed := 0, 0, 0
		var artErr error
		if opts.artwork && !opts.dryRun {
			if !opts.classic {
				artErr = fmt.Errorf("artwork is only supported for the 6G/7G Classic format")
			} else {
				ids := trackIDs(kept)
				links, astats, aerr := artwork.Sync(opts.root, kept, ipod.AlbumKey,
					func(t *meta.Track) uint64 { return ids[t] }, artwork.ClassicFormats())
				if aerr != nil {
					artErr = aerr
				} else {
					artImages = astats.Images
					artAlbums, artFailed = astats.WithArt, astats.Failed
					ipOpts.Artwork = links
					// Rebuild with the artwork fields set; they live in the
					// fixed-size mhit header, so the size is unchanged.
					data, lay, err = ipod.BuildBytes(kept, ipOpts)
				}
			}
		}
		if err != nil {
			send(progress.StockDB{Err: err})
			return
		}
		if opts.dryRun {
			send(progress.StockDB{N: lay.Tracks, Albums: lay.Albums, Dropped: dropped, Classic: opts.classic, Written: false, ShuffleN: shuffleN})
			return
		}
		if err := ipod.WriteDatabase(opts.root, data); err != nil {
			send(progress.StockDB{Err: err})
			return
		}
		send(progress.StockDB{N: lay.Tracks, Albums: lay.Albums, Dropped: dropped, Classic: opts.classic, Written: true, ShuffleN: shuffleN, ArtImages: artImages, ArtAlbums: artAlbums, ArtFailed: artFailed, ArtErr: artErr})
	}
}

// trackIDs maps each track to the 1-based id the iTunesDB builder assigns
// (ids run over the filtered track order), used as the dbid/song_id key
// shared with the ArtworkDB.
func trackIDs(tracks []*meta.Track) map[*meta.Track]uint64 {
	m := make(map[*meta.Track]uint64, len(tracks))
	var id uint64
	for _, t := range tracks {
		if t == nil || t.DevPath == "" {
			continue
		}
		id++
		m[t] = id
	}
	return m
}

func guidOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// readSysInfo reads the plain-text SysInfo file (falling back to the
// SysInfoExtended plist) and returns the device model number and FireWire
// GUID. Both are pure filesystem data; no hardware query is performed.
func readSysInfo(root string) (model, firewire string) {
	deviceDir := filepath.Join(root, "iPod_Control", "Device")

	m, f := parseSysInfoText(filepath.Join(deviceDir, "SysInfo"))
	if m != "" {
		model = m
	}
	if f != "" {
		firewire = f
	}

	// The extended file is an XML plist; only fill in what's still missing.
	m, f = parseSysInfoPlist(filepath.Join(deviceDir, "SysInfoExtended"))
	if model == "" {
		model = m
	}
	if firewire == "" {
		firewire = f
	}
	return model, firewire
}

func parseSysInfoText(path string) (model, firewire string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(key))
		v := strings.TrimSpace(strings.Trim(val, "\x00"))
		switch k {
		case "modelnumstr", "modelnumber", "modelname":
			if model == "" {
				model = normalizeModel(v)
			}
		case "firewireguid":
			if firewire == "" {
				firewire = strings.TrimPrefix(v, "0x")
			}
		}
	}
	return model, firewire
}

// parseSysInfoPlist reads the XML plist variant of SysInfoExtended, extracting
// the ModelNumber and FireWireGUID keys.
func parseSysInfoPlist(path string) (model, firewire string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	text := string(data)
	if model == "" {
		if model = plistValue(text, "ModelNumber"); model == "" {
			model = plistValue(text, "ModelNumStr")
		}
		model = normalizeModel(model)
	}
	if firewire == "" {
		firewire = plistValue(text, "FireWireGUID")
	}
	return model, strings.TrimPrefix(firewire, "0x")
}

// normalizeModel trims a device model string down to the base 5-character
// model code, dropping any region/language suffix (e.g. "MB147LL/A" ->
// "MB147").
func normalizeModel(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	for len(s) > 5 {
		s = s[:len(s)-1]
	}
	return s
}

// plistValue returns the value of the given <key> in an XML plist string, or
// "" if the key is absent.
func plistValue(text, key string) string {
	needle := "<key>" + key + "</key>"
	i := strings.Index(text, needle)
	if i < 0 {
		return ""
	}
	rest := text[i+len(needle):]
	s := strings.Index(rest, "<string>")
	if s < 0 {
		return ""
	}
	rest = rest[s+len("<string>"):]
	e := strings.Index(rest, "</string>")
	if e < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:e])
}

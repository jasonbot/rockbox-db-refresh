package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v2"

	"rbdb/internal/config"
	"rbdb/internal/db"
	"rbdb/internal/ipod"
	"rbdb/internal/progress"
	"rbdb/internal/tui"
)

type stockOptions struct {
	root        string
	dryRun      bool
	classic     bool
	firewire    []byte
	fireWireStr string
	model       string
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
		// yields the FireWire GUID for the 6G/7G HASH58 checksum.
		model, fwStr := readSysInfo(absRoot)
		classic := ipod.IsClassic(model)

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
			root:        absRoot,
			dryRun:      dryRun,
			classic:     classic,
			firewire:    fw,
			fireWireStr: fwStr,
			model:       model,
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
		stockJob(opts)(context.Background(), func(m any) {
			switch msg := m.(type) {
			case progress.Found:
				fmt.Fprintf(os.Stderr, "read %d tracks from the Rockbox database\n", msg.N)
			case progress.StockDB:
				if msg.Err != nil {
					done <- msg.Err
					return
				}
				if opts.classic {
					fmt.Fprintf(os.Stderr, "%d tracks, %d albums -> %s format (HASH58)\n", msg.N, msg.Albums, classicLabel(msg.Classic))
				} else {
					fmt.Fprintf(os.Stderr, "%d tracks, %d albums -> %s format\n", msg.N, msg.Albums, classicLabel(msg.Classic))
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

		if opts.dryRun {
			_, lay, err := ipod.BuildBytes(tracks, ipod.Options{Classic: opts.classic, FireWire: opts.firewire})
			if err != nil {
				send(progress.StockDB{Err: err})
				return
			}
			send(progress.StockDB{N: lay.Tracks, Albums: lay.Albums, Classic: opts.classic, Written: false})
			return
		}

		lay, err := ipod.Build(opts.root, tracks, ipod.Options{Classic: opts.classic, FireWire: opts.firewire})
		if err != nil {
			send(progress.StockDB{Err: err})
			return
		}
		send(progress.StockDB{N: lay.Tracks, Albums: lay.Albums, Classic: opts.classic, Written: true})
	}
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
		k := strings.TrimSpace(key)
		v := strings.TrimSpace(strings.Trim(val, "\x00"))
		switch k {
		case "ModelNumStr", "ModelNumber", "ModelName":
			if model == "" {
				model = normalizeModel(v)
			}
		case "FirewireGuid", "FireWireGUID", "FireWireGuid":
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

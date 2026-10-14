package shuffle

import (
	"bufio"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"rbdb/internal/meta"
)

// Default cap for the generated shuffled playlist. The firmware keeps the
// whole playlist in RAM ("max files in playlist" setting, default 10000 on
// devices with > 1 MB of memory), so we stay just under that.
const ShuffleLimitDefault = 9999

// Playlist catalogue defaults. The firmware's PLAYLIST_CATALOG_DEFAULT_DIR is
// HOME_DIR "Playlists" (firmware/export/rbpaths.h), i.e. /Playlists at the
// volume root on native targets. The menu enumerates this directory live and
// filters it to .m3u/.m3u8 files (playlist_catalog.c display_playlists()),
// so dropping a playlist here is enough to make it appear.
const catalogDefaultDir = "/Playlists"

// PlaylistFileExt is appended to the generated catalogue playlist name. It
// must be .m3u8 so the firmware treats the file as UTF-8 (is_m3u8_name() in
// apps/playlist.c).
const PlaylistFileExt = ".m3u8"

// addedDatesFile persists per-track add dates (unix seconds) as
// "devPath\tseconds" lines. The tagcache format has no add-time tag, so
// this sidecar keeps the recency ordering stable: the first sighting seeds
// from the file ctime and refreshes keep the original value even if ctime
// changes later (tag edits, copies...).
const addedDatesFile = "added.tsv"

func ReadAddedDates(rbDir string) map[string]int64 {
	m := make(map[string]int64)
	f, err := os.Open(filepath.Join(rbDir, addedDatesFile))
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		path, secs, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(secs), 10, 64)
		if err != nil || v <= 0 {
			continue
		}
		m[path] = v
	}
	return m
}

// WriteAddedDates atomically replaces the sidecar with the given entries.
func WriteAddedDates(rbDir string, m map[string]int64) error {
	type kv struct {
		k string
		v int64
	}
	entries := make([]kv, 0, len(m))
	for k, v := range m {
		entries = append(entries, kv{k, v})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].k < entries[j].k })

	tmp := filepath.Join(rbDir, addedDatesFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%d\n", e.k, e.v)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(rbDir, addedDatesFile))
}

// ApplyAddedDates stamps each kept track with its add date: previously seen
// paths keep their stored value; new paths are seeded from the file ctime
// (mtime as fallback). Returns the updated map, pruned to the kept set,
// ready to be written back.
func ApplyAddedDates(rbDir string, tracks []*meta.Track) map[string]int64 {
	stored := ReadAddedDates(rbDir)
	now := time.Now().Unix()
	for _, t := range tracks {
		if v, ok := stored[t.DevPath]; ok && v > 0 {
			t.Added = v
			continue
		}
		switch {
		case t.CTime > 0:
			t.Added = t.CTime
		case t.MTime > 0:
			t.Added = t.MTime
		default:
			t.Added = now
		}
		stored[t.DevPath] = t.Added
	}
	return stored
}

// NewPlaylistName is the catalogue filename used for a freshly generated
// shuffle, e.g. "2026-10-14 09 New Shuffle.m3u8".
func NewPlaylistName(t time.Time) string {
	return t.Format("2006-01-02 15 New Shuffle") + PlaylistFileExt
}

// CatalogDir resolves the host path of the device's playlist catalogue for a
// device mounted at root. It honours the "playlist catalog directory" setting
// in <rbDir>/config.cfg and otherwise falls back to the firmware default
// (/Playlists at the volume root). Rockbox's "<HOME>" placeholder is treated
// as the volume root.
func CatalogDir(root, rbDir string) string {
	dev := readCatalogDir(rbDir)
	if dev == "" {
		dev = catalogDefaultDir
	}
	dev = strings.ReplaceAll(dev, "<HOME>", "/")
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(dev, "/")))
}

// readCatalogDir returns the configured "playlist catalog directory" device
// path, or "" if the setting is absent. Rockbox writes config.cfg as
// "<cfg_name>: <value>" lines with CRLF endings (settings_write_config()).
func readCatalogDir(rbDir string) string {
	f, err := os.Open(filepath.Join(rbDir, "config.cfg"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "playlist catalog directory" {
			return strings.Trim(strings.TrimSpace(v), "\"")
		}
	}
	return ""
}

// WriteCatalogPlaylist shuffles tracks and writes a new UTF-8 playlist into
// the catalogue directory so the firmware lists it in the "Playlists" menu.
// The file is named "<YYYY-MM-DD HH> New Shuffle.m3u8" and holds device-
// absolute track paths. It returns the playlist name and track count.
func WriteCatalogPlaylist(catDir string, tracks []*meta.Track, limit int, recency float64, now time.Time) (string, int, error) {
	if len(tracks) == 0 {
		return "", 0, fmt.Errorf("no tracks to shuffle")
	}
	if limit <= 0 || limit > len(tracks) {
		limit = len(tracks)
	}
	recencyShuffle(tracks, recency)
	picked := make([]string, 0, limit)
	for _, t := range tracks {
		if len(picked) == cap(picked) {
			break
		}
		picked = append(picked, t.DevPath)
	}

	var b []byte
	for _, p := range picked {
		b = append(b, p...)
		b = append(b, '\n')
	}
	if err := os.MkdirAll(catDir, 0755); err != nil {
		return "", 0, err
	}
	name := NewPlaylistName(now)
	if err := os.WriteFile(filepath.Join(catDir, name), b, 0644); err != nil {
		return "", 0, err
	}
	return name, len(picked), nil
}

// recencyShuffle produces a uniformly random permutation when recency <= 0.
// Otherwise it biases newer tracks (higher add date) toward the front using
// a weighted random permutation (Efraimidis-Spirakis): rank r by descending
// add date gets weight 1/(r+1)^recency, each track draws key log(U)/w, and
// the keys are sorted in decreasing order. Every permutation is still
// possible, but the expected position of a track rises with its recency
// rank. Tracks without a recorded add date fall back to ctime.
func recencyShuffle(tracks []*meta.Track, recency float64) {
	added := func(t *meta.Track) int64 {
		if t.Added > 0 {
			return t.Added
		}
		return t.CTime
	}

	if recency <= 0 {
		rand.Shuffle(len(tracks), func(i, j int) {
			tracks[i], tracks[j] = tracks[j], tracks[i]
		})
		return
	}

	order := make([]*meta.Track, len(tracks))
	copy(order, tracks)
	sort.SliceStable(order, func(i, j int) bool { return added(order[i]) > added(order[j]) })

	type keyed struct {
		t *meta.Track
		k float64
	}
	keys := make([]keyed, len(order))
	for r, t := range order {
		w := math.Pow(float64(r+1), -recency)
		u := rand.Float64()
		for u == 0 {
			u = rand.Float64()
		}
		keys[r] = keyed{t, math.Log(u) / w}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].k == keys[j].k {
			return added(keys[i].t) > added(keys[j].t)
		}
		return keys[i].k > keys[j].k
	})
	for i, k := range keys {
		tracks[i] = k.t
	}
}

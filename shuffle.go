package main

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

// Default cap for the generated shuffled playlist. The firmware keeps the
// whole playlist in RAM ("max files in playlist" setting, default 10000 on
// devices with > 1 MB of memory), so we stay just under that.
const shuffleLimitDefault = 9999

// Name of the shuffled playlist written into .rockbox/, referenced by the
// control file below. Must stay an .m3u8 name so the firmware treats it as
// UTF-8 (is_m3u8_name() in apps/playlist.c).
const shufflePlaylistFile = "dynamic.m3u8"

// Control file format (see "Dynamic playlist design" in apps/playlist.c):
// first line is P:<version>:<dir>:<file>; a nonempty <file> makes the
// firmware load the tracks from <dir>/<file> as the current playlist when it
// replays this file in playlist_resume().
func writeShuffledPlaylist(rbDevDir string, rbDir string, tracks []*Track, limit int, recency float64) (int, error) {
	if len(tracks) == 0 {
		return 0, fmt.Errorf("no tracks to shuffle")
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
	if err := os.WriteFile(filepath.Join(rbDir, shufflePlaylistFile), b, 0644); err != nil {
		return 0, err
	}

	control := fmt.Sprintf("P:6:%s:%s\n", rbDevDir, shufflePlaylistFile)
	if err := os.WriteFile(filepath.Join(rbDir, ".playlist_control"), []byte(control), 0644); err != nil {
		return 0, err
	}
	return len(picked), nil
}

// recencyShuffle produces a uniformly random permutation when recency <= 0.
// Otherwise it biases newer tracks (higher CTime) toward the front using a
// weighted random permutation (Efraimidis-Spirakis): rank r by descending
// ctime gets weight 1/(r+1)^recency, each track draws key log(U)/w, and the
// keys are sorted in decreasing order. Every permutation is still possible,
// but the expected position of a track rises with its recency rank.
func recencyShuffle(tracks []*Track, recency float64) {
	if recency <= 0 {
		rand.Shuffle(len(tracks), func(i, j int) {
			tracks[i], tracks[j] = tracks[j], tracks[i]
		})
		return
	}

	order := make([]*Track, len(tracks))
	copy(order, tracks)
	sort.SliceStable(order, func(i, j int) bool { return order[i].CTime > order[j].CTime })

	type keyed struct {
		t *Track
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
			return keys[i].t.CTime > keys[j].t.CTime
		}
		return keys[i].k > keys[j].k
	})
	for i, k := range keys {
		tracks[i] = k.t
	}
}

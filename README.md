# rbdb — build a Rockbox tagcache from Go

This is two things: notes on how Rockbox's music database works, and `rbdb`,
a single static Go binary that builds one for you. Point it at a (mounted)
player, it scans the music files, reads their metadata, and writes a complete
database into `<root>/.rockbox/`.

## How the on-device builder works

Rockbox's database ("tagcache") is built by a background thread in
`apps/tagcache.c`. A scan walks the configured search roots, probes every file
with a known audio extension via the metadata library (`get_metadata`), and
appends each track to `database_tmp.tcd` as one `temp_file_entry` followed by
its raw string tags.

A commit then turns that into the final files: every non-numeric tag gets its
own `database_<tag>.tcd` (deduplicated and case-insensitively sorted, except
filename which is dumped as-is), and numeric values go straight into the master
index `database_idx.tcd` — where playcount/rating stats of previously deleted
entries get resurrected by matching filename or artist+album+title CRC32
hashes. The ASCII changelog `database_changelog.txt` is only an interchange
format for runtime statistics; you don't need it for a working database.

The Go port produces exactly what the device would after a complete rebuild
(or an incremental merge with `-refresh`), so the firmware loads it directly
and can later run its own incremental update on top.

### Files in `.rockbox/`

- `database_idx.tcd` — master header + one fixed-size index entry per track
- `database_<N>.tcd` — string data for tag N, non-numeric tags only
- `database_tmp.tcd` — transient; must NOT exist when the device boots

Non-numeric tags are 0..8 plus 12:

```
0 artist      5 composer        8 grouping
1 album       6 comment        12 canonicalartist (virtual: artist||albumartist)
2 genre       7 albumartist
3 title       4 filename  <-- special: unsorted, not deduplicated
```

Everything else is numeric and lives inline in the master index:

```
9 year         13 bitrate     16 rating       19 commitid      22 lastoffset
10 discnumber  14 length(ms)  17 playtime     20 mtime
11 tracknumber 15 playcount   18 lastplayed   21 lastelapsed
```

(`TAG_COUNT = 23`; see `enum tag_type` in `apps/tagcache.h`.)

### Binary layout

All little-endian; the firmware auto-detects foreign-endian databases by
byte-swapping the magic (`TAGCACHE_SUPPORT_FOREIGN_ENDIAN`).

Every file starts with `struct tagcache_header` (12 bytes):

```c
int32 magic;       // 0x54434810 ('TCH' v16) -- TAGCACHE_MAGIC
int32 datasize;    // payload bytes after this header
int32 entry_count;
```

Master file `database_idx.tcd`: the header above followed by `serial` (0),
`commitid` (increments per commit, 1 after first build), and `dirty` (must be
0 or the firmware refuses the DB) — then one index entry per track, 96 bytes
each: `tag_seek[23]` (byte offset into the per-tag file for string tags, the
value itself for numeric ones) and a `flag` word.

Per-tag string files `database_<N>.tcd`: the header, then one `tagfile_entry`
per record — `tag_length`, `idx_id`, and a NUL-terminated UTF-8 string.
Sorted tags pad records with `'X'` so `(8 + tag_length)` is a multiple of 8
(`TAGFILE_ENTRY_CHUNK_LENGTH`); the filename tag doesn't pad and stores
exactly `strlen(path)+1`, one record per track in master-index order.

Unique tags (everything except title and filename) are deduplicated
case-insensitively and stored case-insensitively sorted, with `<Untagged>`
sorting before everything else; their records use `idx_id = -1`. Title is
sorted but not unique; filename is neither. For every non-unique sorted-tag
entry with `idx_id >= 0` (i.e. title) and every filename record,
`indices[idx_id].tag_seek[tag]` must point at exactly that record —
`load_tagcache()`/`check_all_headers()` enforce all of this at boot, and any
violation disables the DB until rebuild.

Scanning applies some fallbacks (mirroring `add_tagcache()`): empty/missing
tags become `<Untagged>`, canonicalartist falls back to albumartist, grouping
falls back to *title* (!), missing tracknumber is `-1` and discnumber is `0`,
mtime is the file's unix mtime.

Master `datasize` is `24 + 96*entry_count + Σ datasize(database_<N>.tcd)` over
all non-numeric tags except filename.

At boot the firmware checks all headers; if they pass the DB is ready and can
be loaded wholesale into RAM (ramcache). If `database_tmp.tcd` exists from an
interrupted commit, the user gets asked whether to finish it. With auto-update
enabled, the device can rescan and append/delete incrementally afterwards — a
pre-built external DB participates just like a native one.

## The Go port

```sh
go build -o rbdb .            # inside utils/tagcache-go
```

### Usage

```
rbdb [flags] [root]
```

`root` is the device root (mount point). Resolution order when omitted:
explicit flag/arg → interactive filepicker (TUI) or last-used root from the
cache (non-TUI) → `.`. The cache lives at
`~/.config/rockbox-db-refresh/last.txt`; every run with a valid root rewrites
it.

| Flag | Default | Effect |
|---|---|---|
| `-root DIR` | | device root directory (same as positional arg) |
| `-dry-run` | off | scan + parse only, write nothing |
| `-refresh` | off | incremental update, see below |
| `-shuffle` | off | install a shuffled library playlist after building, see below |
| `-shuffle-limit N` | 9999 | max tracks in the shuffled playlist (firmware default cap is 10000) |
| `-shuffle-recency F` | 2.0 | recency bias strength for `-shuffle`; 0 = uniform shuffle |
| `-max-tag BYTES` | 512 | rune-safe truncation for non-path string tags; paths keep up to 1024 bytes |
| `-no-tui` | off | plain progress output (also used automatically when piped) |
| `-v` | off | verbose output |

### Patterns

```sh
./rbdb /media/player              # TUI full rebuild of <root>/.rockbox
./rbdb -refresh /media/player     # incremental: parse only new/changed files
./rbdb -dry-run -refresh /media/player  # preview the kept/updated/added/removed delta
./rbdb -shuffle /media/player     # rebuild + install shuffled playlist
./rbdb -no-tui /media/player      # non-interactive (cron etc.)
```

Without an explicit root on a TTY, a filepicker opens at the last-used
directory (`d` picks the current dir, `enter` selects/open, `q` quits); the
build never auto-starts. Non-TUI runs without a root silently default to the
cached one.

### Scan

`<root>` is walked recursively, skipping dot-directories. Picked extensions:
mp3 mp2 flac ogg oga opus m4a m4b mp4 ape mpc wv. Metadata parsing covers
ID3v1/v2.2–2.4 (incl. unsync), APEv2, FLAC/Vorbis comments, Ogg Vorbis/Opus
comments and MP4/iTunes atoms (incl. trailing-`moov` layouts); duration and
bitrate come from FLAC STREAMINFO, MPEG frames/Xing, Ogg granule positions or
MP4 `mvhd` where available. Stored paths are device paths like
`/Music/foo.mp3`, derived from the file's location below the root.

### What gets written

- `database_idx.tcd`, `database_0..8.tcd`, `database_12.tcd` — the database,
  exactly what the device would produce after a complete rebuild; stale
  `database_tmp.tcd` is removed.
- With `-shuffle`: `dynamic.m3u8` (shuffled library, capped by
  `-shuffle-limit`) plus `.playlist_control` referencing it — the same pair
  the firmware uses for the current dynamic playlist, so the player boots
  into the shuffled library as its current playlist (actually resuming
  playback still depends on autoresume/bookmark settings).
  `added.tsv` records per-track add dates for stable shuffle ordering.
- Elsewhere: `~/.config/rockbox-db-refresh/last.txt` remembers the root.

### Refresh (`-refresh`)

The previous database is read back and merged with a fresh scan:

- files whose device path exists **and** whose mtime is unchanged are carried
  over without parsing — play counts, ratings, playtime and other runtime
  statistics survive;
- changed or new files are re-parsed; paths no longer found are dropped;
- if no usable existing database is present it degrades to a full rebuild.

### Shuffle (`-shuffle`)

Recency-weighted random permutation (Efraimidis–Spirakis): tracks ranked by
add date get weight `1/(rank+1)^-shuffle-recency` and are sampled without
replacement, so recently added music drifts toward the front while every
ordering stays possible. Add dates are seeded from each file's ctime on first
sight and then kept stable across refreshes in `.rockbox/added.tsv` (ctime
changes whenever a tag is rewritten; the add date shouldn't).
`-shuffle-recency 0` gives a uniform shuffle.

The TUI shows an overall progress bar with ETA, live parsed/skipped/kept
counts, a status table of all output files, a log pane (skipped files, refresh
delta), and a cancel button (`c`); cancellation stops cleanly at the next file
boundary without writing anything.

Robustness: strings are sanitized to C-string semantics (truncated at embedded
NULs, control chars stripped, trimmed) so dedup/sort/record sizes match what
the firmware reads back. Malformed files are logged as `SKIP`, never abort the
run. All on-disk fields are 32-bit by definition; offsets and sizes are
checked against that ceiling instead of wrapping.

Pure Go stdlib, no cgo, single static binary.

Known gaps vs. the C implementation: no duration parsing for APE/MPC/WV (they
get length 0), legacy codepages (e.g. Shift-JIS tags) are stored verbatim as
bytes, rendered however the player's codepage setting says. `-refresh` keys
reuse off mtime only — touching a file without changing its tags forces a
re-parse (harmless, just slower); statistics of files that fail to re-parse
are lost.

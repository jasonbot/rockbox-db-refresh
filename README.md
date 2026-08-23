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

The Go port implements a fresh full rebuild: it produces exactly what the
device would after a complete rebuild, so the firmware loads it directly and
can later run its own incremental update on top.

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
./rbdb /media/player          # interactive TUI, writes <root>/.rockbox
./rbdb -no-tui /media/player  # plain progress output (also used when piped)
./rbdb -dry-run /media/player # scan + parse only, prints what would be written
./rbdb -shuffle /media/player # also install a shuffled library playlist
```

It scans `<root>` recursively (skipping dot-directories), picks up files with
extensions mp3 mp2 flac ogg oga opus m4a m4b mp4 ape mpc wv, parses their
metadata — ID3v1/ID3v2.2–2.4 incl. unsync, APEv2, FLAC/Vorbis comments, Ogg
Vorbis/Opus comments, MP4/iTunes atoms incl. trailing-`moov` layouts —
computes duration/bitrate where the container provides it (FLAC STREAMINFO,
MPEG frames/Xing, Ogg granule positions, MP4 `mvhd`), and writes
`database_idx.tcd` plus `database_0..8.tcd` and `database_12.tcd` into
`<root>/.rockbox/`, removing any stale `database_tmp.tcd`. Stored paths are
device paths like `/Music/foo.mp3`, derived from the file's location below
`-root`.

The TUI (default on a TTY, built with Bubble Tea/bubbles) shows a progress
bar with current file and counts, a table of output files with live status,
and a scrollable log pane listing unparseable files. Cancel with `c` or the
button; the job stops cleanly at the next file boundary without writing a
partial database.

With `-shuffle`, after a successful build the tool also writes
`<root>/.rockbox/dynamic.m3u8` — a shuffled playlist of the whole library,
capped by `-shuffle-limit` (default 9999) — plus `.playlist_control`
referencing it, the same pair the firmware uses for the current dynamic
playlist. On next boot the player has the shuffled library as its current
playlist; actually resuming playback still depends on autoresume/bookmark
settings, and the firmware caps playlists at its "max files in playlist"
setting anyway (default 10000). The shuffle is recency-weighted: tracks are
ranked by ctime and sampled without replacement with weight
`1/(rank+1)^-shuffle-recency` (Efraimidis–Spirakis), so recently added files
tend toward the front while every ordering stays possible. `-shuffle-recency 0`
gives a plain uniform shuffle.

Robustness: strings are sanitized to C-string semantics (truncated at embedded
NULs, control chars stripped, trimmed) so dedup/sort/record sizes match what
the firmware reads back. Malformed files are logged as `SKIP`, never abort the
run. All on-disk fields are 32-bit by definition; offsets and sizes are
checked against that ceiling instead of wrapping. Non-path tags are truncated
rune-safely (`-max-tag`, default 512 bytes); paths keep up to 1024 bytes since
the device must match them verbatim during incremental updates.

Pure Go stdlib, no cgo, single static binary.

Known gaps vs. the C implementation: no duration parsing for APE/MPC/WV (they
get length 0), runtime statistics start at zero (no changelog import — back up
`.rockbox/database_*` if your device has playcounts you care about, or let the
device update itself instead), and legacy codepages (e.g. Shift-JIS tags) are
stored verbatim as bytes, rendered however the player's codepage setting says.

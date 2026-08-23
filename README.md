# Rockbox TagCache: on-device database format & Go port (`rbdb`)

This directory contains:

1. **Documentation** of the system that generates the Rockbox music database
   on the device (implemented in `apps/tagcache.c`, `apps/tagcache.h` in the
   Rockbox source tree).
2. **A Go port** of that system as a single static executable (`rbdb`) that can
   be pointed at the root of a (mounted) player, will find all supported music
   files, read their metadata, and write a complete, ready-to-use database into
   `<root>/.rockbox/`.

---

## Part 1 — How the on-device database builder works

### 1.1 Architecture

Rockbox's database ("tagcache") is built by a background thread in
`apps/tagcache.c`. The pipeline is:

```
 file scan            temporary DB                commit
+-----------+   +----------------------+   +--------------------------+
| check_dir |-->| database_tmp.tcd     |-->| build_index()  per tag   |
| add_tagcache   | (one full record per |   | tempbuf_sort() dedup/sort|
|  (get_metadata)|  track, strings +    |   | build_numeric_indices()  |
|                |  numerics inline)    |   +-------------+------------+
+-----------+   +----------------------+                 |
                                                   <root>/.rockbox/
                                                   database_idx.tcd   (master index)
                                                   database_0.tcd ... (per-tag data)
```

* A **scan** walks the configured search roots. Every file with a known audio
  extension is probed and parsed by the metadata library (`get_metadata`).
  Each track is appended to `database_tmp.tcd` as one `temp_file_entry`
  followed by its raw string tags.
* A **commit** then:
  * for every non-numeric tag builds `database_<tag>.tcd`
    (deduplication + case-insensitive sort for "unique"/"sorted" tags,
    plain sequential dump for the filename tag), and
  * writes numeric values directly into the master index
    (`database_idx.tcd`), resurrecting playcount/rating statistics of
    previously deleted entries by matching filename or artist+album+title
    CRC32 hashes.
* The ASCII changelog `database_changelog.txt` ("## Changelog version 1")
  is only an interchange format for runtime statistics (playcount etc.);
  it is not required for a working database.

The Go port implements the equivalent of a fresh **full rebuild**: it produces
the same final files the device would produce after a complete rebuild, so the
firmware loads them directly (and can later run its own incremental
"Update database" on top of them).

### 1.2 Files produced (in `.rockbox/`)

| File                  | Contents                                                        |
|-----------------------|-----------------------------------------------------------------|
| `database_idx.tcd`    | master header + one fixed-size index entry per track            |
| `database_<N>.tcd`    | string data for tag `N`, only for non-numeric tags              |
| `database_tmp.tcd`    | transient; must NOT exist when the device boots                 |

Non-numeric tags are 0..8 and 12:

```
0 artist      5 composer        8 grouping
1 album       6 comment        12 canonicalartist (virtual: artist||albumartist)
2 genre       7 albumartist
3 title       4 filename  <-- special: unsorted, not deduplicated
```

All other tags are **numeric** and stored inline in the master index:

```
9 year         13 bitrate     16 rating       19 commitid      22 lastoffset
10 discnumber  14 length(ms)  17 playtime     20 mtime
11 tracknumber 15 playcount   18 lastplayed   21 lastelapsed
```

(`TAG_COUNT = 23`; see `enum tag_type` in `apps/tagcache.h`.)

### 1.3 Binary layout

Everything is little-endian here; the firmware auto-detects foreign endian
databases by byte-swapping the magic (`TAGCACHE_SUPPORT_FOREIGN_ENDIAN`).

Common header (`struct tagcache_header`, 12 bytes) — used by every file:

```c
int32 magic;      // 0x54434810 ('TCH' v16)  -- TAGCACHE_MAGIC
int32 datasize;   // bytes of payload after this header
int32 entry_count;// number of entries in this file
```

Master file `database_idx.tcd`:

```
master header (24 bytes):
    tagcache_header tch;   // as above, entry_count = number of tracks
    int32 serial;          // 0
    int32 commitid;        // increments per commit; 1 after first build
    int32 dirty;           // must be 0, else firmware refuses the DB

entry_count × index_entry (96 bytes each):
    int32 tag_seek[23];    // string tags: byte offset into database_<tag>.tcd
                           // numeric tags: the value itself
    int32 flag;            // FLAG_DELETED|FLAG_DIRCACHE|FLAG_DIRTYNUM|...
```

Per-tag string files `database_<N>.tcd`:

```
tagcache_header (12 bytes)          // entry_count = records below
entry_count × tagfile_entry:
    int32 tag_length;               // length incl. terminating '\0'
                                    // (+ padding, see below)
    int32 idx_id;                   // -1 for unique tags; else master index id
    char  tag_data[tag_length];     // NUL-terminated UTF-8
```

Two record flavours exist:

* **sorted tags** (all except `filename`): records are padded with `'X'` so
  that `(8 + tag_length)` is a multiple of 8 (`TAGFILE_ENTRY_CHUNK_LENGTH`);
  `tag_length` includes the padding.
* **filename tag** (`database_4.tcd`): records are *not* padded;
  `tag_length` = `strlen(path)+1`. One record per track, in master-index
  order, `idx_id` = track's master index.

### 1.4 Semantics the firmware relies on

These are enforced by `load_tagcache()` / `check_all_headers()`; a database
that violates any of them is rejected (music DB disabled until rebuild):

1. All files carry the correct magic; `dirty == 0`.
2. For every non-unique sorted-tag entry with `idx_id >= 0` (i.e. `title`),
   `indices[idx_id].tag_seek[tag]` must equal the byte offset of exactly that
   record. Same for every `filename` record.
3. Unique tags (artist, album, genre, composer, comment, albumartist,
   grouping, canonicalartist) are **deduplicated case-insensitively** and
   stored **case-insensitively sorted**, with the literal string
   `<Untagged>` sorting before everything else. Their records use
   `idx_id = -1`.
4. `title` is sorted but not unique (duplicate records allowed); `filename`
   is neither.
5. Numeric values are plain signed 32-bit ints inside `tag_seek[]`.

Fallbacks applied while scanning (mirroring `add_tagcache()`):

* empty/missing string tags become `<Untagged>`
* canonicalartist = artist if present, else albumartist
* grouping = grouping if present, else title (!)
* missing tracknumber is stored as `-1`, missing discnumber as `0`
* `mtime` = file modification time (unix seconds)

Master `datasize` is defined as
`24 + 96 * entry_count + Σ datasize(database_<N>.tcd)` over all non-numeric
tags **except filename**.

### 1.5 Boot-time behaviour

At boot the firmware checks all headers (`check_all_headers()`). If they pass,
the DB is marked ready and optionally loaded wholesale into RAM (ramcache).
If `database_tmp.tcd` exists (interrupted commit), the user is asked whether
to finish it. With "auto-update" enabled the device can later rescan and
append/delete entries incrementally using the changelog/resurrection logic —
a pre-built external DB participates in this like a native one.

---

## Part 2 — The Go port (`rbdb`)

### 2.1 Usage

```sh
go build -o rbdb .          # inside utils/tagcache-go
./rbdb /media/player        # interactive TUI, writes <root>/.rockbox
./rbdb -no-tui /media/player  # plain progress output (also used when piped)
./rbdb -dry-run /media/player # scan + parse only, prints what would be written
```

Interactive mode (default on a TTY) is built with
[Bubble Tea](https://github.com/charmbracelet/bubbletea)/[bubbles](https://github.com/charmbracelet/bubbles)
and shows:

* an overall progress bar, current file, parsed/skipped counts and elapsed time,
* a table of every output file (`database_idx.tcd`, `database_0.tcd`, …)
  with its purpose and live status,
* a scrollable log pane in the lower half (mouse wheel or arrow keys)
  listing unparseable files and phase transitions,
* a clickable `[ Cancel ]` button (or press `c`); the job stops cleanly at
  the next file boundary without writing a partial database.

The tool:

1. recursively scans `<root>` (skipping dot-directories and `.rockbox`),
2. finds files with extensions: `mp3 mp2 flac ogg oga opus m4a m4b mp4 ape
   mpc wv`,
3. parses metadata (ID3v1/ID3v2.2–2.4 incl. unsync, APEv2, FLAC/Vorbis
   comments, Ogg Vorbis/Opus comments, MP4/iTunes atoms incl. trailing
   `moov` layouts),
4. computes duration/bitrate where the container provides it
   (FLAC STREAMINFO, MPEG frames/Xing, Ogg granule positions, MP4 `mvhd`),
5. writes `database_idx.tcd` and `database_0.tcd … database_8.tcd`,
   `database_12.tcd` into `<root>/.rockbox/`, removing any stale
   `database_tmp.tcd`.

Paths stored in the DB are device paths (`/Music/foo.mp3`), derived from the
file's location below `-root`.

### 2.2 Robustness

* All metadata strings are sanitized to C-string semantics (truncated at
  embedded NULs, control characters stripped, trimmed) so deduplication,
  sorting and record sizes stay consistent — matching how the C firmware
  reads them back.
* A malformed file never aborts the run: it is logged as `SKIP` and omitted.
* Every on-disk field is 32-bit by definition; the writer checks offsets and
  data sizes against that ceiling and fails loudly instead of wrapping.
* Non-path tags are truncated (`-max-tag`, default 512 bytes, rune-safe);
  paths are kept up to 1024 bytes because the device must match them
  verbatim during incremental updates.


Pure Go standard library; single static binary, no cgo.

### 2.3 Limitations vs. the C implementation

* Formats without an easily parseable duration (APE/MPC/WV) get length 0.
* Runtime statistics (playcount/rating/lastplayed…) are initialised to 0;
  there is no changelog import. If the device already has a DB with stats you
  want to keep, back up `.rockbox/database_*` before running, or let the
  device update instead of rebuilding externally.
* Legacy codepages (e.g. Shift-JIS tags) are stored verbatim as bytes; the
  player's codepage setting governs how they render.

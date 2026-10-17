# rbdb — Rockbox database builder and audio converter

A single static Go binary that builds Rockbox tagcache databases, fixes MP3
metadata/album art in-place, and syncs audio libraries between directories.
No cgo, no external dependencies beyond ffmpeg (for MP3 encoding).

## Commands

```
rbdb <command> [options]
```

| Command           | Purpose                                                      |
| ----------------- | ------------------------------------------------------------ |
| `db`              | Build/update the `.rockbox/` tagcache database               |
| `fix`             | Fix MP3 metadata and album art in-place                      |
| `sync`            | Convert audio files from a source directory to a destination |
| `sync-stock-db`   | Write an iPod stock iTunesDB from the Rockbox tagcache       |

### rbdb db

Scans a Rockbox device root, reads metadata from all audio files, and writes a
complete tagcache database to `<root>/.rockbox/`.

```
rbdb db [options] [root]
```

| Flag                 | Default | Effect                                                   |
| -------------------- | ------- | -------------------------------------------------------- |
| `-root DIR`          |         | device root directory (same as positional arg)           |
| `-dry-run`           | off     | scan + parse only, write nothing                         |
| `-refresh`           | off     | incremental update (reuse unchanged files, drop deleted) |
| `-shuffle`           | off     | create a new shuffled playlist in the playlist catalogue |
| `-shuffle-limit N`   | 9999    | max tracks in shuffled playlist                          |
| `-shuffle-recency F` | 2.0     | recency bias (0 = uniform)                               |
| `-max-tag BYTES`     | 512     | truncation limit for non-path string tags                |
| `-no-tui`            | off     | plain progress output                                    |
| `-v`                 | off     | verbose output                                           |

Root resolution (when omitted): interactive filepicker (TUI) or last-used root
from `~/.config/rockbox-db-refresh/last.txt` → `.`.

```sh
rbdb db /media/player              # TUI full rebuild
rbdb db -refresh /media/player     # incremental update
rbdb db -shuffle /media/player     # rebuild + new catalogue playlist
```

### rbdb fix

Fixes MP3 metadata and album art in-place using MusicBrainz for normalization
and cover art fetching. Only processes files that need changes.

```
rbdb fix [options] <path>
```

| Flag              | Default | Effect                                              |
| ----------------- | ------- | --------------------------------------------------- |
| `-path DIR`       |         | directory containing MP3s                           |
| `-dry-run`        | off     | scan only, write nothing                            |
| `-normalize MODE` | none    | metadata normalization: `none`, `fill`, `overwrite` |
| `-no-art`         | off     | skip cover art fetching                             |
| `-min-score N`    | 50      | minimum MusicBrainz search score (0–100)            |
| `-no-tui`         | off     | plain output                                        |
| `-v`              | off     | verbose output                                      |

- `fill` only fills empty fields (won't overwrite existing tags)
- `overwrite` replaces all metadata with MusicBrainz data
- Uses ffmpeg `-c copy` to rewrite tags without re-encoding audio
- Corrupt embedded art (undecodable APIC payloads left by broken taggers) is
  treated as missing and refetched rather than preserved

```sh
rbdb fix /path/to/mp3s             # interactive fix with defaults
rbdb fix -normalize fill /music    # fill missing metadata + fetch art
rbdb fix -normalize overwrite /music  # replace all metadata
```

### rbdb sync

Converts audio files from an origin directory to a destination, transcoding to
MP3 with optional MusicBrainz metadata normalization and cover art.

```
rbdb sync [options] <origin> <destination>
```

| Flag               | Default | Effect                                              |
| ------------------ | ------- | --------------------------------------------------- |
| `-origin DIR`      |         | source directory                                    |
| `-destination DIR` |         | output directory                                    |
| `-dry-run`         | off     | scan only                                           |
| `-overwrite`       | off     | overwrite existing files                            |
| `-delete`          | off     | remove destination files not in origin              |
| `-update`          | off     | refresh the device's `.rockbox` tagcache after sync |
| `-normalize MODE`  | none    | metadata normalization: `none`, `fill`, `overwrite` |
| `-no-art`          | off     | skip cover art fetching                             |
| `-min-score N`     | 50      | minimum MusicBrainz search score                    |
| `-no-tui`          | off     | plain output                                        |
| `-v`               | off     | verbose output                                      |

Embedded art is normalized to what players actually display: decodable art is
re-encoded to a baseline (non-progressive) JPEG, and corrupt payloads left by
broken taggers are dropped (and refetched when art fetching is enabled)
instead of being copied verbatim.

```sh
rbdb sync /music/library /mnt/player/Music
rbdb sync -overwrite -normalize fill /music /mnt/player/Music
```

### rbdb sync-stock-db

Writes a stock Apple iTunesDB from the Rockbox tagcache database so a stock
iPod firmware sees your library. Targets the iPod Classic 5G/6G/7G: Rockbox
paths become iTunes `:Music:…` locations, with tracks, album grouping (the
type-4 album database) and the master library playlist, and playcounts.

```
rbdb sync-stock-db [options] [root]
```

| Flag                 | Default    | Effect                                                     |
| -------------------- | ---------- | ---------------------------------------------------------- |
| `-root DIR`          |            | device root directory (same as positional arg)             |
| `-dry-run`           | off        | parse + generate in memory only, write nothing             |
| `-firewire-guid HEX` |            | device FireWire GUID; auto-read from SysInfo if omitted    |
| `-classic`           | off        | force 6G/7G Classic format (skip model detection)          |
| `-stock-max-bytes N` | 58000000   | iTunesDB byte budget; oversized libraries are rotated      |
| `-stock-recency F`   | 2.0        | rotation bias: newer albums kept more likely; 0 = uniform  |
| `-no-stock-rotate`   | off        | fail instead of rotating when over the byte budget         |
| `-artwork`           | off        | write ArtworkDB + ithmb cover files (Cover Flow; Classic)  |
| `-no-tui`            | off        | plain output                                               |
| `-v`                 | off        | verbose output                                             |

The output format is detected from the device model number in the SysInfo file
(`iPod_Control/Device/SysInfo`), the same way a syncing app reads the hardware:
6G/7G Classic models (e.g. MB029, MB147, MB150, MB562, MC293) get the Classic
format (dbv 0x30, header 0xF4) with the device-bound HASH58 HMAC-SHA1 checksum;
a 5G/5.5G Video model (MA…, or no SysInfo) gets the 5G format (dbv 0x0f, header
0x68) with no checksum. This is filesystem data only — no device query.

The Classic firmware parses the whole iTunesDB into RAM at boot, and its
budget is small: on the 7G Classic a ~56.6 MiB database hangs at the Apple
logo while ~56.0 MiB boots. When the library would not fit the byte budget
(`-stock-max-bytes`, default 58,000,000), whole albums are rotated out — an
Efraimidis–Spirakis weighted draw biased by add date (the same scheme as
`db --shuffle`, sharing `.rockbox/added.tsv`), so each sync keeps a fresh,
album-intact sample of the library and growth breaks nothing. The dropped
count is reported after the run; use `-no-stock-rotate` to fail instead.

The Classic checksum needs the FireWire GUID, read automatically from SysInfo
(`FirewireGuid` key), or overridable with `-firewire-guid`. The 5G format needs
no GUID. The iTunesDB is written to `<root>/iPod_Control/iTunes/iTunesDB`.

With `-artwork` (Classic only), embedded album art is extracted per album from
the MP3 files (deduplicated by content, shared across compilation albums),
converted to the device's RGB565 ithmb formats (56/128/320 px), and written to
`<root>/iPod_Control/Artwork/` (`ArtworkDB` + `F*.ithmb`) so Cover Flow and the
Now Playing screen show covers. Tracks are linked via the mhit `mhii_link`
field, which lives in the fixed-size header, so artwork never changes the
iTunesDB size or the rotation budget.

```sh
rbdb sync-stock-db /media/classic          # format + GUID detected from SysInfo
rbdb sync-stock-db -classic /media/iPod    # force 6G/7G Classic format
rbdb sync-stock-db -firewire-guid 0x… /mnt # override the GUID
rbdb sync-stock-db -dry-run /media/iPod    # validate/report, write nothing
rbdb sync-stock-db -stock-max-bytes 56000000 /media/classic  # tighter budget
rbdb sync-stock-db -artwork /media/classic # also write Cover Flow artwork
```

## MusicBrainz integration

Both `fix` and `sync` can query the MusicBrainz Search API to:

1. **Normalize metadata** — fill missing tags or overwrite with canonical data
2. **Fetch album art** — download front cover from the Cover Art Archive

A disk-backed cache (`~/.cache/rockbox-converter/musicbrainz/`) stores API
responses (SHA256 keys, 30-day TTL). Rate limiting: 1 request/sec.

## Metadata normalization modes

- `none` — no MusicBrainz queries (default)
- `fill` — fill empty fields only (artist, album, title)
- `overwrite` — replace all metadata with MusicBrainz canonical data

## TUI

All commands show an interactive terminal UI when connected to a TTY (unless
`-no-tui` or `-dry-run` is set). The TUI displays:

- Progress bar with ETA
- Live stats (parsed/skipped/failed)
- Log pane with file-by-file status
- Cancel button (press `c` or click)

When the job finishes, the button turns green and the TUI waits for a keypress
before exiting — you can review logs and stats at your leisure.

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
hashes.

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
sorted but not unique; filename is neither.

## Layout

```
main.go                  entry point
cmd/                     CLI subcommands (db, fix, sync, sync-stock-db) via urfave/cli
internal/meta/           Track type + tag parsers (ID3, APE, FLAC/Vorbis, Ogg, MP4)
internal/db/             tagcache writer (Build) and reader (ReadDatabase)
internal/ipod/           iTunesDB serializer (mhbd/mhit/mhod/mhia/mhyp/mhip), HASH58, device-type detection
internal/shuffle/        shuffled catalogue playlist, recency weighting, added.tsv store
internal/tui/            interactive terminal UI (progress, log, cancel)
internal/config/         last-root cache
internal/progress/       message types piped from pipeline to UIs
internal/convert/        ffmpeg-based MP3 encoding
internal/walker/         directory walking and file discovery
internal/artwork/        cover art processing, ID3 APIC embed/extract, ArtworkDB/ithmb writer
internal/musicbrainz/    MusicBrainz API client, cover art archive, metadata normalization
```

## Building

```sh
go build -o rbdb .
```

Pure Go stdlib (plus bubbletea for TUI and urfave/cli for argument parsing),
no cgo, single static binary. Requires ffmpeg on `$PATH for MP3 encoding.

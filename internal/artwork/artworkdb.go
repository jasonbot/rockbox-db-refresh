package artwork

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"rbdb/internal/meta"
)

// Header sizes of the ArtworkDB chunk family (from real iPod Classic
// ArtworkDB files, matching libgpod's db-artwork-writer.c).
const (
	mhfdHdr = 132
	mhsdHdr = 96
	mhliHdr = 92
	mhlaHdr = 92
	mhlfHdr = 92
	mhiiHdr = 152
	mhodHdr = 24
	mhniHdr = 76
	mhifHdr = 124
)

// ArtworkDir is the on-device directory of ArtworkDB and the ithmb files.
const ArtworkDir = "iPod_Control/Artwork"

// ArtLink links one track to its artwork image in the ArtworkDB.
type ArtLink struct {
	ImgID   uint32 // mhii image_id, referenced from the track's mhii_link
	SrcSize uint32 // byte size of the original embedded image
}

// Stats summarizes a Sync run.
type Stats struct {
	Albums    int // album groups considered
	WithArt   int // albums where embedded art was found
	Images    int // unique images written to the ArtworkDB
	Failed    int // images dropped because conversion failed
	IthmbSize int64
}

// Sync extracts embedded album art for the given tracks, writes the ithmb
// pixel files and the ArtworkDB under <root>/iPod_Control/Artwork, and
// returns the per-track artwork links to embed in the iTunesDB.
//
// Artwork is shared album-wide (first track with art wins for the whole
// album) and deduplicated by content across albums, matching the sparse
// artwork scheme iTunes/libgpod use on the Classic. songID maps a track to
// its 64-bit iTunesDB id (used as the representative song_id of each image).
//
// The ArtworkDB and ithmbs are fully rewritten on every run; files from
// previous runs with foreign format ids are removed.
func Sync(root string, tracks []*meta.Track, albumKey func(*meta.Track) string, songID func(*meta.Track) uint64, formats []Format) (map[*meta.Track]ArtLink, Stats, error) {
	var st Stats
	links := map[*meta.Track]ArtLink{}
	if len(tracks) == 0 {
		return links, st, nil
	}

	// Group tracks into albums, first-seen order.
	type album struct {
		tracks []*meta.Track
	}
	index := map[string]int{}
	var albums []*album
	for _, t := range tracks {
		k := albumKey(t)
		i, ok := index[k]
		if !ok {
			i = len(albums)
			index[k] = i
			albums = append(albums, &album{})
		}
		albums[i].tracks = append(albums[i].tracks, t)
	}
	st.Albums = len(albums)

	// Phase 1: extract embedded art per album (first track carrying art).
	artByAlbum := make([][]byte, len(albums))
	var extMu sync.Mutex
	var eg errgroup.Group
	eg.SetLimit(6)
	for i, alb := range albums {
		i, alb := i, alb
		eg.Go(func() error {
			for _, t := range alb.tracks {
				p := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(t.DevPath, "/")))
				img, err := ExtractMP3Cover(p)
				if err == nil && len(img) > 0 {
					extMu.Lock()
					artByAlbum[i] = img
					extMu.Unlock()
					return nil
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, st, err
	}

	// Phase 2: dedupe by content, keeping first-seen order.
	type unique struct {
		data   []byte
		repTrk *meta.Track
	}
	var uniques []unique
	hashIdx := map[[sha1.Size]byte]int{}
	imgOfAlbum := make([]int, len(albums))
	for i, alb := range albums {
		imgOfAlbum[i] = -1
		img := artByAlbum[i]
		if len(img) == 0 {
			continue
		}
		st.WithArt++
		h := sha1.Sum(img)
		id, ok := hashIdx[h]
		if !ok {
			id = len(uniques)
			hashIdx[h] = id
			uniques = append(uniques, unique{data: img, repTrk: alb.tracks[0]})
		}
		imgOfAlbum[i] = id
	}
	if len(uniques) == 0 {
		return links, st, nil
	}

	// Phase 3: convert and write pixel data. Each image lives at
	// index*entrySize within each format's ithmb file; failed conversions
	// leave an unreferenced hole, which is harmless.
	artDir := filepath.Join(root, ArtworkDir)
	if err := os.MkdirAll(artDir, 0755); err != nil {
		return nil, st, err
	}
	cleanStaleIthmbs(artDir, formats)

	files := make([]*os.File, len(formats))
	for i, f := range formats {
		p := filepath.Join(artDir, ithmbName(f.ID))
		fp, err := os.Create(p)
		if err != nil {
			return nil, st, err
		}
		files[i] = fp
	}
	ok := make([]bool, len(uniques))
	var wrMu sync.Mutex
	var eg2 errgroup.Group
	eg2.SetLimit(6)
	for idx, u := range uniques {
		idx, u := idx, u
		eg2.Go(func() error {
			for fi, f := range formats {
				pix, err := ToRGB565(u.data, f.Width, f.Height)
				if err != nil {
					wrMu.Lock()
					st.Failed++
					wrMu.Unlock()
					return nil // leave hole, no link
				}
				wrMu.Lock()
				_, werr := files[fi].WriteAt(pix, int64(idx)*int64(f.EntrySize()))
				wrMu.Unlock()
				if werr != nil {
					return werr
				}
			}
			ok[idx] = true
			return nil
		})
	}
	werr := eg2.Wait()
	var total int64
	for i, fp := range files {
		if fi, err := fp.Stat(); err == nil {
			total += fi.Size()
		}
		fp.Close()
		// Truncate away trailing holes from failed tail conversions.
		trim := int64(len(uniques)) * int64(formats[i].EntrySize())
		_ = os.Truncate(filepath.Join(artDir, ithmbName(formats[i].ID)), trim)
	}
	if werr != nil {
		return nil, st, werr
	}
	st.IthmbSize = total

	// Phase 4: build ArtworkDB from the successful images.
	var entries []artEntry
	for idx, u := range uniques {
		if !ok[idx] {
			continue
		}
		entries = append(entries, artEntry{
			imgID:   idx + 1,
			songID:  songID(u.repTrk),
			srcSize: len(u.data),
			idx:     idx,
		})
	}
	st.Images = len(entries)

	db := buildArtworkDB(entries, formats)
	if err := os.WriteFile(filepath.Join(artDir, "ArtworkDB"), db, 0644); err != nil {
		return nil, st, err
	}

	// Phase 5: per-track links.
	imgOK := map[int]int{} // unique index -> image id
	for _, e := range entries {
		imgOK[e.idx] = e.imgID
	}
	for i, alb := range albums {
		uid := imgOfAlbum[i]
		if uid < 0 {
			continue
		}
		imgID, yes := imgOK[uid]
		if !yes {
			continue
		}
		link := ArtLink{ImgID: uint32(imgID), SrcSize: uint32(len(uniques[uid].data))}
		for _, t := range alb.tracks {
			links[t] = link
		}
	}
	return links, st, nil
}

func ithmbName(formatID int) string { return fmt.Sprintf("F%d_1.ithmb", formatID) }

// cleanStaleIthmbs removes ithmb files that do not belong to the current
// format set (e.g. written by a previous run or a foreign tool).
func cleanStaleIthmbs(dir string, formats []Format) {
	keep := map[string]bool{}
	for _, f := range formats {
		keep[ithmbName(f.ID)] = true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, "F") && strings.HasSuffix(name, ".ithmb") && !keep[name] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// artEntry is one unique image for the ArtworkDB image list.
type artEntry struct {
	imgID   int
	songID  uint64
	srcSize int
	idx     int // position within each ithmb file (offset = idx*entrySize)
}

// buildArtworkDB assembles the ArtworkDB binary:
//
//	mhfd
//	  mhsd type=1 → mhli → mhii[unique image] → mhod type=2 → mhni → mhod type=3 (per format)
//	  mhsd type=2 → mhla (empty; album list is photo-only)
//	  mhsd type=3 → mhlf → mhif[format]
func buildArtworkDB(entries []artEntry, formats []Format) []byte {
	var images []byte
	for _, e := range entries {
		images = append(images, buildMhii(e.imgID, e.songID, e.srcSize, e.idx, formats)...)
	}
	mhli := chunkHdr("mhli", mhliHdr)
	binary.LittleEndian.PutUint32(mhli[0x08:], uint32(len(entries)))
	ds1 := wrapArtMhsd(1, append(mhli, images...))

	mhla := chunkHdr("mhla", mhlaHdr) // album list: photo-only, empty for music
	ds2 := wrapArtMhsd(2, mhla)

	var files []byte
	for _, f := range formats {
		files = append(files, buildMhif(f)...)
	}
	mhlf := chunkHdr("mhlf", mhlfHdr)
	binary.LittleEndian.PutUint32(mhlf[0x08:], uint32(len(formats)))
	ds3 := wrapArtMhsd(3, append(mhlf, files...))

	body := append(append(ds1, ds2...), ds3...)

	hdr := make([]byte, mhfdHdr)
	copy(hdr, "mhfd")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], mhfdHdr)
	u.PutUint32(hdr[0x08:], uint32(mhfdHdr+len(body)))
	u.PutUint32(hdr[0x10:], 2)
	u.PutUint32(hdr[0x14:], 3) // dataset count
	nextID := 1
	if len(entries) > 0 {
		nextID = entries[len(entries)-1].imgID + 1
	}
	u.PutUint32(hdr[0x1C:], uint32(nextID))
	u.PutUint32(hdr[0x30:], 2)
	return append(hdr, body...)
}

func buildMhii(imgID int, songID uint64, srcSize, idx int, formats []Format) []byte {
	var children []byte
	for _, f := range formats {
		children = append(children, buildMhodContainer(f, idx)...)
	}
	hdr := make([]byte, mhiiHdr)
	copy(hdr, "mhii")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], mhiiHdr)
	u.PutUint32(hdr[0x08:], uint32(mhiiHdr+len(children)))
	u.PutUint32(hdr[0x0C:], uint32(len(formats)))
	u.PutUint32(hdr[0x10:], uint32(imgID))
	u.PutUint64(hdr[0x14:], songID)
	u.PutUint32(hdr[0x30:], uint32(srcSize))
	return append(hdr, children...)
}

// buildMhodContainer wraps an mhni in the type-2 ("location") mhod.
func buildMhodContainer(f Format, idx int) []byte {
	mhni := buildMhni(f, idx)
	hdr := make([]byte, mhodHdr)
	copy(hdr, "mhod")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], mhodHdr)
	u.PutUint32(hdr[0x08:], uint32(mhodHdr+len(mhni)))
	u.PutUint16(hdr[0x0C:], 2)
	return append(hdr, mhni...)
}

func buildMhni(f Format, idx int) []byte {
	name := buildMhodFilename(":" + ithmbName(f.ID))
	hdr := make([]byte, mhniHdr)
	copy(hdr, "mhni")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], mhniHdr)
	u.PutUint32(hdr[0x08:], uint32(mhniHdr+len(name)))
	u.PutUint32(hdr[0x0C:], 1) // one child (the filename mhod)
	u.PutUint32(hdr[0x10:], uint32(f.ID))
	u.PutUint32(hdr[0x14:], uint32(idx*f.EntrySize())) // offset in ithmb file
	u.PutUint32(hdr[0x18:], uint32(f.EntrySize()))
	u.PutUint16(hdr[0x20:], uint16(f.Height))
	u.PutUint16(hdr[0x22:], uint16(f.Width))
	u.PutUint32(hdr[0x28:], uint32(f.EntrySize()))
	return append(hdr, name...)
}

// buildMhodFilename writes the type-3 mhod holding the ithmb device path,
// UTF-16LE encoded (encoding byte 2), 4-byte-padded, as on real Classics.
func buildMhodFilename(s string) []byte {
	var enc []byte
	for _, r := range s {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(r))
		enc = append(enc, b[:]...)
	}
	pad := (4 - len(enc)%4) % 4

	body := make([]byte, 0, 12+len(enc)+pad)
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(enc)))
	body = append(body, tmp[:]...)
	body = append(body, 2)          // encoding: UTF-16
	body = append(body, 0, 0, 0)    //
	body = append(body, 0, 0, 0, 0) //
	body = append(body, enc...)
	body = append(body, make([]byte, pad)...)

	hdr := make([]byte, mhodHdr)
	copy(hdr, "mhod")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], mhodHdr)
	u.PutUint32(hdr[0x08:], uint32(mhodHdr+len(body)))
	u.PutUint16(hdr[0x0C:], 3)
	return append(hdr, body...)
}

func buildMhif(f Format) []byte {
	hdr := make([]byte, mhifHdr)
	copy(hdr, "mhif")
	u := binary.LittleEndian
	u.PutUint32(hdr[0x04:], mhifHdr)
	u.PutUint32(hdr[0x08:], mhifHdr)
	u.PutUint32(hdr[0x10:], uint32(f.ID))
	u.PutUint32(hdr[0x14:], uint32(f.EntrySize()))
	return hdr
}

func wrapArtMhsd(typ int, child []byte) []byte {
	b := chunkHdr("mhsd", mhsdHdr)
	binary.LittleEndian.PutUint32(b[0x08:], uint32(mhsdHdr+len(child)))
	binary.LittleEndian.PutUint16(b[0x0C:], uint16(typ))
	return append(b, child...)
}

// chunkHdr builds a zeroed header with the chunk id and header length set;
// the caller fills the count/length field at 0x08 as appropriate.
func chunkHdr(id string, hdr int) []byte {
	b := make([]byte, hdr)
	copy(b, id)
	binary.LittleEndian.PutUint32(b[0x04:], uint32(hdr))
	return b
}

package musicbrainz

import (
	"context"
	"rbdb/internal/artwork"
	"rbdb/internal/meta"
	"rbdb/internal/progress"
)

// EnrichOptions controls MusicBrainz metadata normalization and cover art fetching.
type EnrichOptions struct {
	Mode     string // "none", "fill", or "overwrite"
	FetchArt bool
	MinScore int
}

// EnrichResult reports what changed on a track during enrichment.
type EnrichResult struct {
	Match           *NormalizeResult
	Normalized      bool
	ArtworkFetched  bool
}

// Enrich calls MusicBrainz to normalize metadata and optionally fetch cover art.
// It sends progress messages for artwork fetched and metadata normalized events.
// The track is modified in place; the caller is responsible for persisting changes.
func Enrich(ctx context.Context, client *Client, track *meta.Track, path string, opts EnrichOptions, send func(any)) (*EnrichResult, error) {
	if client == nil {
		return nil, nil
	}

	result := &EnrichResult{}

	// Do a single MusicBrainz lookup; reuse it for both normalize and art.
	if opts.Mode != "none" || opts.FetchArt {
		match, err := client.NormalizeMetadata(ctx, track, SearchOptions{MinScore: opts.MinScore})
		if err != nil || match == nil || !match.HasMatch {
			return result, err
		}
		result.Match = match

		// Normalize metadata
		if opts.Mode != "none" {
			ApplyNormalization(track, match, opts.Mode)
			result.Normalized = true
			send(progress.MetadataNormalized{Path: path})
		}

		// Fetch cover art if track has none
		if opts.FetchArt && track.CoverArt == nil {
			raw, _, artErr := client.FetchFrontCover(ctx, match.ReleaseID, match.ReleaseGroupID)
			if artErr == nil && raw != nil {
				processed, procErr := artwork.ProcessCoverArt(raw, artwork.DefaultMaxDim, artwork.DefaultMaxArtFileSize)
				if procErr == nil {
					track.CoverArt = processed
					track.CoverArtMIME = "image/jpeg"
					result.ArtworkFetched = true
					send(progress.ArtworkFetched{Path: path})
				}
			}
		}
	}

	return result, nil
}

// ApplyNormalization overwrites or fills track metadata from a MusicBrainz match.
func ApplyNormalization(track *meta.Track, match *NormalizeResult, mode string) {
	switch mode {
	case "overwrite":
		track.Meta = match.Meta
		if match.Year > 0 {
			track.Year = match.Year
		}
		if match.TrackNumber > 0 {
			track.TrackNum = match.TrackNumber
		}
		if match.DiscNumber > 0 {
			track.Disc = match.DiscNumber
		}
	case "fill":
		if track.Meta.Title == "" && match.Meta.Title != "" {
			track.Meta.Title = match.Meta.Title
		}
		if track.Meta.Artist == "" && match.Meta.Artist != "" {
			track.Meta.Artist = match.Meta.Artist
		}
		if track.Meta.Album == "" && match.Meta.Album != "" {
			track.Meta.Album = match.Meta.Album
		}
	}
}

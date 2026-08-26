package musicbrainz

import (
	"context"

	"rbdb/internal/meta"
)

type NormalizeResult struct {
	Meta           meta.Meta
	Year           int
	DiscNumber     int
	TrackNumber    int
	HasMatch       bool
	MatchScore     int
	ReleaseID      string
	ReleaseGroupID string
}

func (c *Client) NormalizeMetadata(ctx context.Context, t *meta.Track, opts SearchOptions) (*NormalizeResult, error) {
	match, err := c.SearchRecording(ctx, t.Meta.Artist, t.Meta.Title, t.Meta.Album, opts)
	if err != nil {
		return &NormalizeResult{HasMatch: false}, err
	}
	if match == nil {
		return &NormalizeResult{HasMatch: false}, nil
	}

	return &NormalizeResult{
		Meta: meta.Meta{
			Title:  normalizeWhitespace(match.Title),
			Artist: normalizeWhitespace(match.Artist),
			Album:  normalizeWhitespace(match.Album),
		},
		Year:           match.Year,
		DiscNumber:     match.DiscNumber,
		TrackNumber:    match.TrackNumber,
		HasMatch:       true,
		MatchScore:     match.Score,
		ReleaseID:      match.ReleaseID,
		ReleaseGroupID: match.ReleaseGroupID,
	}, nil
}

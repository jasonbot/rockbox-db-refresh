package musicbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type RecordingMatch struct {
	RecordingID    string
	ReleaseID      string
	ReleaseGroupID string
	Title          string
	Artist         string
	Album          string
	Year           int
	DiscNumber     int
	TrackNumber    int
	Score          int
}

type SearchOptions struct {
	MinScore int
	Limit    int
}

type mbRecordingSearchResult struct {
	Recordings []mbRecording `json:"recordings"`
}

type mbRecording struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Score        int              `json:"score"`
	ArtistCredit []mbArtistCredit `json:"artist-credits"`
	Releases     []mbRelease      `json:"releases"`
}

type mbArtistCredit struct {
	Name string `json:"name"`
}

type mbRelease struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Date         string         `json:"date"`
	ReleaseGroup mbReleaseGroup `json:"release-group"`
}

type mbReleaseGroup struct {
	ID   string `json:"id"`
	Type string `json:"primary-type"`
}

func (c *Client) SearchRecording(ctx context.Context, artist, title, album string, opts SearchOptions) (*RecordingMatch, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}

	if opts.Limit <= 0 {
		opts.Limit = 5
	}
	if opts.MinScore <= 0 {
		opts.MinScore = 50
	}

	query := fmt.Sprintf(`recording:"%s" AND artist:"%s"`, title, artist)
	params := url.Values{
		"query": {query},
		"fmt":   {"json"},
		"limit": {fmt.Sprintf("%d", opts.Limit)},
	}
	searchURL := fmt.Sprintf("%s/recording?%s", mbBaseURL, params.Encode())

	key := cacheKey(searchURL)
	if data, ok := c.cacheGet(key); ok {
		return parseRecordingSearch(data, opts)
	}

	resp, err := c.doGet(ctx, searchURL)
	if err != nil {
		return nil, fmt.Errorf("musicbrainz search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("musicbrainz search: HTTP %d", resp.StatusCode)
	}

	var buf json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&buf); err != nil {
		return nil, fmt.Errorf("musicbrainz search decode: %w", err)
	}
	c.cachePut(key, buf)

	return parseRecordingSearch(buf, opts)
}

func parseRecordingSearch(data []byte, opts SearchOptions) (*RecordingMatch, error) {
	var result mbRecordingSearchResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("musicbrainz parse: %w", err)
	}

	var best *RecordingMatch
	for _, r := range result.Recordings {
		score := r.Score
		if score < opts.MinScore {
			continue
		}

		artistName := ""
		if len(r.ArtistCredit) > 0 {
			artistName = r.ArtistCredit[0].Name
		}

		match := &RecordingMatch{
			RecordingID: r.ID,
			Title:       r.Title,
			Artist:      artistName,
			Score:       score,
		}

		if len(r.Releases) > 0 {
			match.ReleaseID = r.Releases[0].ID
			match.Album = r.Releases[0].Title
			if r.Releases[0].ReleaseGroup.ID != "" {
				match.ReleaseGroupID = r.Releases[0].ReleaseGroup.ID
			}
			if d := r.Releases[0].Date; len(d) >= 4 {
				fmt.Sscanf(d[:4], "%d", &match.Year)
			}
		}

		if best == nil || score > best.Score {
			best = match
		}
	}

	return best, nil
}

func (c *Client) LookupRelease(ctx context.Context, releaseMBID string) (string, error) {
	if err := c.wait(ctx); err != nil {
		return "", err
	}

	params := url.Values{
		"inc": {"release-groups"},
		"fmt": {"json"},
	}
	lookupURL := fmt.Sprintf("%s/release/%s?%s", mbBaseURL, releaseMBID, params.Encode())

	resp, err := c.doGet(ctx, lookupURL)
	if err != nil {
		return "", fmt.Errorf("musicbrainz lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("musicbrainz lookup: HTTP %d", resp.StatusCode)
	}

	var release struct {
		ReleaseGroup struct {
			ID string `json:"id"`
		} `json:"release-group"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("musicbrainz lookup decode: %w", err)
	}

	return release.ReleaseGroup.ID, nil
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

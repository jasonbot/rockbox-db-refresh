package musicbrainz

import (
	"context"
	"fmt"
	"io"
)

func (c *Client) FetchFrontCover(ctx context.Context, releaseMBID, releaseGroupMBID string) ([]byte, string, error) {
	if releaseMBID != "" {
		data, mime, err := c.fetchCover(ctx, caaBaseURL+"/release/"+releaseMBID+"/front-500")
		if err == nil && data != nil {
			return data, mime, nil
		}
	}

	if releaseGroupMBID != "" {
		data, mime, err := c.fetchCover(ctx, caaBaseURL+"/release-group/"+releaseGroupMBID+"/front-500")
		if err == nil && data != nil {
			return data, mime, nil
		}
	}

	return nil, "", nil
}

func (c *Client) fetchCover(ctx context.Context, coverURL string) ([]byte, string, error) {
	key := cacheKey(coverURL)
	if data, ok := c.cacheGet(key); ok {
		return data, detectMIME(data), nil
	}

	if err := c.wait(ctx); err != nil {
		return nil, "", err
	}

	resp, err := c.doGet(ctx, coverURL)
	if err != nil {
		return nil, "", fmt.Errorf("CAA fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, "", nil
	}
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("CAA returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("CAA read: %w", err)
	}

	c.cachePut(key, data)
	return data, detectMIME(data), nil
}

func detectMIME(data []byte) string {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xd8 {
		return "image/jpeg"
	}
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		return "image/png"
	}
	return "image/jpeg"
}

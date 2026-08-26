package musicbrainz

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const cacheTTL = 30 * 24 * time.Hour

func cacheKey(url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%x", h)
}

func (c *Client) cacheGet(key string) ([]byte, bool) {
	path := filepath.Join(c.cacheDir, key)
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > cacheTTL {
		os.Remove(path)
		return nil, false
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

func (c *Client) cachePut(key string, data []byte) {
	os.WriteFile(filepath.Join(c.cacheDir, key), data, 0644)
}

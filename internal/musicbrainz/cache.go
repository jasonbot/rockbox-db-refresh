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

	c.mu.RLock()
	info, err := os.Stat(path)
	if err != nil {
		c.mu.RUnlock()
		return nil, false
	}
	if time.Since(info.ModTime()) <= cacheTTL {
		data, err := os.ReadFile(path)
		c.mu.RUnlock()
		return data, err == nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	info, err = os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > cacheTTL {
		os.Remove(path)
	}
	return nil, false
}

func (c *Client) cachePut(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	os.WriteFile(filepath.Join(c.cacheDir, key), data, 0644)
}

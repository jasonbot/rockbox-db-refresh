package musicbrainz

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	DefaultUserAgent = "rockbox-converter/1.0 ( https://github.com/jscheier/rockbox )"
	mbBaseURL        = "https://musicbrainz.org/ws/2"
	caaBaseURL       = "https://coverartarchive.org"
)

type Client struct {
	http     *http.Client
	limiter  *rate.Limiter
	cacheDir string
	mu       sync.RWMutex
}

func NewClient(userAgent, cacheDir string) (*Client, error) {
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		cacheDir = filepath.Join(home, ".cache", "rockbox-converter", "musicbrainz")
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	transport := &userAgentTransport{
		Base: http.DefaultTransport,
		UA:   userAgent,
	}

	return &Client{
		http:     &http.Client{Transport: transport, Timeout: 30 * time.Second},
		limiter:  rate.NewLimiter(1, 1),
		cacheDir: cacheDir,
	}, nil
}

func (c *Client) wait(ctx context.Context) error {
	return c.limiter.Wait(ctx)
}

func (c *Client) doGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

type userAgentTransport struct {
	Base http.RoundTripper
	UA   string
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", t.UA)
	return t.Base.RoundTrip(req)
}

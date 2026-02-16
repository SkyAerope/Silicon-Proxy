package source

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

type URLSource struct {
	id         string
	url        string
	interval   time.Duration
	withPrefix bool
	client     *http.Client
}

func NewURLSource(url string, interval time.Duration, withPrefix bool) *URLSource {
	return &URLSource{
		id:         buildSourceID("url", url),
		url:        url,
		interval:   interval,
		withPrefix: withPrefix,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (source *URLSource) ID() string {
	return source.id
}

func (source *URLSource) Interval() time.Duration {
	return source.interval
}

func (source *URLSource) Fetch() ([]string, error) {
	resp, err := source.client.Get(source.url)
	if err != nil {
		return nil, fmt.Errorf("fetch url source: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("url source status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read url source body: %w", err)
	}

	return ParseAndValidateLines(string(body), source.withPrefix), nil
}

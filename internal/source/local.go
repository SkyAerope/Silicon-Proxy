package source

import (
	"fmt"
	"os"
	"time"
)

type LocalSource struct {
	id         string
	path       string
	interval   time.Duration
	withPrefix bool
	useRegex   bool
}

func NewLocalSource(path string, interval time.Duration, withPrefix bool, useRegex bool) *LocalSource {
	return &LocalSource{
		id:         buildSourceID("local", path),
		path:       path,
		interval:   interval,
		withPrefix: withPrefix,
		useRegex:   useRegex,
	}
}

func (source *LocalSource) ID() string {
	return source.id
}

func (source *LocalSource) Interval() time.Duration {
	return source.interval
}

func (source *LocalSource) Fetch() ([]string, error) {
	content, err := os.ReadFile(source.path)
	if err != nil {
		return nil, fmt.Errorf("read local source: %w", err)
	}

	return ParseAndValidateLines(string(content), source.withPrefix, source.useRegex), nil
}

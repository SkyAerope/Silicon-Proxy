package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen            string            `json:"listen"`
	Backend           string            `json:"backend"`
	MaxAuthPerProxy   int               `json:"max_auth_per_proxy"`
	MaxRequestRetries int               `json:"max_request_retries"`
	MaxLiveProxies    int               `json:"max_live_proxies"`
	RequestTimeout    string            `json:"request_timeout"`
	Redis             RedisConfig       `json:"redis"`
	Sources           []SourceConfig    `json:"sources"`
	HealthCheck       HealthCheckConfig `json:"health_check"`
	Pool              PoolConfig        `json:"pool"`

	RequestTimeoutDur time.Duration `json:"-"`
}

type RedisConfig struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	DB       int    `json:"db"`
}

type SourceConfig struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Path       string `json:"path"`
	Interval   string `json:"interval"`
	WithPrefix bool   `json:"with_prefix"`

	IntervalDur time.Duration `json:"-"`
}

type HealthCheckConfig struct {
	Interval    string `json:"interval"`
	Timeout     string `json:"timeout"`
	Target      string `json:"target"`
	MaxFailures int    `json:"max_failures"`
	Concurrency int    `json:"concurrency"`

	IntervalDur time.Duration `json:"-"`
	TimeoutDur  time.Duration `json:"-"`
}

type PoolConfig struct {
	MaxIdleConns        int    `json:"max_idle_conns"`
	MaxIdleConnsPerHost int    `json:"max_idle_conns_per_host"`
	IdleTimeout         string `json:"idle_timeout"`

	IdleTimeoutDur time.Duration `json:"-"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	clean := stripJSONC(data)

	var cfg Config
	if err := json.Unmarshal(clean, &cfg); err != nil {
		return nil, fmt.Errorf("parse config jsonc: %w", err)
	}

	applyDefaults(&cfg)
	if err := validateAndParseDurations(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Backend == "" {
		cfg.Backend = "https://api.siliconflow.cn/"
	}
	if cfg.MaxAuthPerProxy <= 0 {
		cfg.MaxAuthPerProxy = 1
	}
	if cfg.MaxAuthPerProxy > 5 {
		cfg.MaxAuthPerProxy = 5
	}
	if cfg.MaxRequestRetries <= 0 {
		cfg.MaxRequestRetries = 3
	}
	if cfg.MaxLiveProxies < 0 {
		cfg.MaxLiveProxies = 0
	}
	if cfg.RequestTimeout == "" {
		cfg.RequestTimeout = "10s"
	}

	if cfg.Redis.Addr == "" {
		cfg.Redis.Addr = "127.0.0.1:6379"
	}

	if len(cfg.Sources) == 0 {
		cfg.Sources = []SourceConfig{
			{
				Type:       "url",
				URL:        "https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks5.txt",
				Interval:   "3h",
				WithPrefix: false,
			},
		}
	}

	if cfg.HealthCheck.Interval == "" {
		cfg.HealthCheck.Interval = "60s"
	}
	if cfg.HealthCheck.Timeout == "" {
		cfg.HealthCheck.Timeout = "5s"
	}
	if cfg.HealthCheck.Target == "" {
		cfg.HealthCheck.Target = "backend"
	}
	if cfg.HealthCheck.MaxFailures <= 0 {
		cfg.HealthCheck.MaxFailures = 3
	}
	if cfg.HealthCheck.Concurrency <= 0 {
		cfg.HealthCheck.Concurrency = 50
	}

	if cfg.Pool.MaxIdleConns <= 0 {
		cfg.Pool.MaxIdleConns = 200
	}
	if cfg.Pool.MaxIdleConnsPerHost <= 0 {
		cfg.Pool.MaxIdleConnsPerHost = 100
	}
	if cfg.Pool.IdleTimeout == "" {
		cfg.Pool.IdleTimeout = "90s"
	}

	for index := range cfg.Sources {
		if cfg.Sources[index].Interval == "" {
			cfg.Sources[index].Interval = "3h"
		}
	}
}

func validateAndParseDurations(cfg *Config) error {
	for index := range cfg.Sources {
		source := &cfg.Sources[index]
		source.Type = strings.ToLower(strings.TrimSpace(source.Type))

		switch source.Type {
		case "url":
			if source.URL == "" {
				return fmt.Errorf("sources[%d].url is required when type=url", index)
			}
		case "local":
			if source.Path == "" {
				return fmt.Errorf("sources[%d].path is required when type=local", index)
			}
		default:
			return fmt.Errorf("sources[%d].type must be url/local", index)
		}

		dur, err := time.ParseDuration(source.Interval)
		if err != nil {
			return fmt.Errorf("sources[%d].interval parse error: %w", index, err)
		}
		source.IntervalDur = dur
	}

	intervalDur, err := time.ParseDuration(cfg.HealthCheck.Interval)
	if err != nil {
		return fmt.Errorf("health_check.interval parse error: %w", err)
	}
	timeoutDur, err := time.ParseDuration(cfg.HealthCheck.Timeout)
	if err != nil {
		return fmt.Errorf("health_check.timeout parse error: %w", err)
	}
	idleDur, err := time.ParseDuration(cfg.Pool.IdleTimeout)
	if err != nil {
		return fmt.Errorf("pool.idle_timeout parse error: %w", err)
	}
	requestTimeoutDur, err := time.ParseDuration(cfg.RequestTimeout)
	if err != nil {
		return fmt.Errorf("request_timeout parse error: %w", err)
	}

	if intervalDur <= 0 || timeoutDur <= 0 || idleDur <= 0 || requestTimeoutDur <= 0 {
		return errors.New("health/pool durations must be greater than 0")
	}

	cfg.HealthCheck.IntervalDur = intervalDur
	cfg.HealthCheck.TimeoutDur = timeoutDur
	cfg.Pool.IdleTimeoutDur = idleDur
	cfg.RequestTimeoutDur = requestTimeoutDur

	return nil
}

func stripJSONC(input []byte) []byte {
	out := make([]byte, 0, len(input))
	inString := false
	escaped := false
	inLineComment := false
	inBlockComment := false

	for index := 0; index < len(input); index++ {
		current := input[index]
		var next byte
		if index+1 < len(input) {
			next = input[index+1]
		}

		if inLineComment {
			if current == '\n' {
				inLineComment = false
				out = append(out, current)
			}
			continue
		}

		if inBlockComment {
			if current == '*' && next == '/' {
				inBlockComment = false
				index++
			}
			continue
		}

		if inString {
			out = append(out, current)
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				inString = false
			}
			continue
		}

		if current == '"' {
			inString = true
			out = append(out, current)
			continue
		}

		if current == '/' && next == '/' {
			inLineComment = true
			index++
			continue
		}

		if current == '/' && next == '*' {
			inBlockComment = true
			index++
			continue
		}

		out = append(out, current)
	}

	return out
}

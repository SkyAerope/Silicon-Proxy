package pool

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/SkyAerope/Silicon-Proxy/internal/config"
	"golang.org/x/net/proxy"
)

type TransportPool struct {
	requestTimeout time.Duration
	poolConfig     config.PoolConfig
	transports     sync.Map
}

func NewTransportPool(cfg *config.Config) *TransportPool {
	return &TransportPool{
		requestTimeout: cfg.HealthCheck.TimeoutDur,
		poolConfig:     cfg.Pool,
	}
}

func (pool *TransportPool) Get(proxyAddr string) (*http.Transport, error) {
	if existing, ok := pool.transports.Load(proxyAddr); ok {
		transport, ok := existing.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("invalid cached transport type for %s", proxyAddr)
		}
		return transport, nil
	}

	dialContext, err := buildSOCKS5DialContext(proxyAddr, pool.requestTimeout)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		DialContext:           dialContext,
		MaxIdleConns:          pool.poolConfig.MaxIdleConns,
		MaxIdleConnsPerHost:   pool.poolConfig.MaxIdleConnsPerHost,
		IdleConnTimeout:       pool.poolConfig.IdleTimeoutDur,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	actual, _ := pool.transports.LoadOrStore(proxyAddr, transport)
	return actual.(*http.Transport), nil
}

func buildSOCKS5DialContext(proxyAddr string, timeout time.Duration) (func(context.Context, string, string) (net.Conn, error), error) {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, dialer)
	if err != nil {
		return nil, fmt.Errorf("create socks5 dialer failed: %w", err)
	}

	if contextDialer, ok := socksDialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext, nil
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type dialResult struct {
			conn net.Conn
			err  error
		}

		resultChannel := make(chan dialResult, 1)
		go func() {
			conn, dialErr := socksDialer.Dial(network, address)
			resultChannel <- dialResult{conn: conn, err: dialErr}
		}()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-resultChannel:
			return result.conn, result.err
		}
	}, nil
}

package pool

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/SkyAerope/Silicon-Proxy/internal/config"
	"golang.org/x/net/proxy"
)

type TransportPool struct {
	defaultConnectTimeout time.Duration
	poolConfig            config.PoolConfig
	transports            sync.Map
}

func NewTransportPool(cfg *config.Config) *TransportPool {
	return &TransportPool{
		defaultConnectTimeout: cfg.RequestTimeoutDur,
		poolConfig:            cfg.Pool,
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

	dialContext := buildSOCKS5DialContext(proxyAddr, pool.defaultConnectTimeout)

	transport := &http.Transport{
		DialContext:           dialContext,
		DialTLSContext:        buildSOCKS5DialTLSContext(proxyAddr, pool.defaultConnectTimeout),
		MaxIdleConns:          pool.poolConfig.MaxIdleConns,
		MaxIdleConnsPerHost:   pool.poolConfig.MaxIdleConnsPerHost,
		IdleConnTimeout:       pool.poolConfig.IdleTimeoutDur,
		TLSHandshakeTimeout:   0,
		ExpectContinueTimeout: 1 * time.Second,
	}

	actual, _ := pool.transports.LoadOrStore(proxyAddr, transport)
	return actual.(*http.Transport), nil
}

type connectTimeoutContextKey struct{}

// WithConnectTimeout stores a per-request "connect budget" in context.
// It only affects dialing/handshake, not response streaming.
func WithConnectTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if ctx == nil || timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, connectTimeoutContextKey{}, timeout)
}

func connectTimeoutFromContext(ctx context.Context, defaultTimeout time.Duration) time.Duration {
	if ctx == nil {
		return defaultTimeout
	}
	value := ctx.Value(connectTimeoutContextKey{})
	if value == nil {
		return defaultTimeout
	}
	if timeout, ok := value.(time.Duration); ok && timeout > 0 {
		return timeout
	}
	return defaultTimeout
}

func buildSOCKS5DialContext(proxyAddr string, defaultTimeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		timeout := connectTimeoutFromContext(ctx, defaultTimeout)
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		return dialSOCKS5WithBudget(ctx, proxyAddr, network, address, timeout)
	}
}

func buildSOCKS5DialTLSContext(proxyAddr string, defaultTimeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		budget := connectTimeoutFromContext(ctx, defaultTimeout)
		if budget <= 0 {
			budget = 10 * time.Second
		}

		start := time.Now()
		conn, err := dialSOCKS5WithBudget(ctx, proxyAddr, network, address, budget)
		if err != nil {
			return nil, err
		}

		remaining := budget - time.Since(start)
		if remaining <= 0 {
			_ = conn.Close()
			return nil, context.DeadlineExceeded
		}

		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			host = address
		}
		if net.ParseIP(host) != nil {
			host = ""
		}

		_ = conn.SetDeadline(time.Now().Add(remaining))
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.Handshake(); err != nil {
			_ = tlsConn.Close()
			return nil, err
		}
		_ = tlsConn.SetDeadline(time.Time{})
		return tlsConn, nil
	}
}

func dialSOCKS5WithBudget(ctx context.Context, proxyAddr, network, address string, budget time.Duration) (net.Conn, error) {
	forward := &net.Dialer{Timeout: budget, KeepAlive: 30 * time.Second}
	socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, forward)
	if err != nil {
		return nil, fmt.Errorf("create socks5 dialer failed: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	if contextDialer, ok := socksDialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(dialCtx, network, address)
	}

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
	case <-dialCtx.Done():
		return nil, dialCtx.Err()
	case result := <-resultChannel:
		return result.conn, result.err
	}
}

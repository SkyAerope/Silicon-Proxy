package health

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

type HealthStore interface {
	GetProxies(ctx context.Context) ([]string, error)
	MarkProxySuccess(ctx context.Context, proxyAddr string) error
	IncrementProxyFailure(ctx context.Context, proxyAddr string) (int, error)
	RemoveProxyCascade(ctx context.Context, proxyAddr string) error
}

type Checker struct {
	store       HealthStore
	logger      *slog.Logger
	interval    time.Duration
	timeout     time.Duration
	target      string
	maxFailures int
	concurrency int
	running     atomic.Bool
}

type runCounters struct {
	checked atomic.Int64
	success atomic.Int64
	failed  atomic.Int64
	removed atomic.Int64
}

type deadlineDialer struct {
	timeout time.Duration
	dialer  net.Dialer
}

func (dialer deadlineDialer) Dial(network, addr string) (net.Conn, error) {
	conn, err := dialer.dialer.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(dialer.timeout))
	return conn, nil
}

func NewChecker(store HealthStore, logger *slog.Logger, interval, timeout time.Duration, target string, maxFailures, concurrency int) *Checker {
	return &Checker{
		store:       store,
		logger:      logger,
		interval:    interval,
		timeout:     timeout,
		target:      target,
		maxFailures: maxFailures,
		concurrency: concurrency,
	}
}

func (checker *Checker) Run(ctx context.Context) {
	checker.RunOnce(ctx)

	ticker := time.NewTicker(checker.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checker.RunOnce(ctx)
		}
	}
}

func (checker *Checker) RunOnce(ctx context.Context) {
	if !checker.running.CompareAndSwap(false, true) {
		return
	}

	startTime := time.Now()
	totalProxies := 0
	counters := &runCounters{}

	checker.logger.Info(
		"health: run started",
		"target", checker.target,
		"timeout", checker.timeout.String(),
		"max_failures", checker.maxFailures,
		"concurrency", checker.concurrency,
	)

	defer func() {
		checker.running.Store(false)
		checker.logger.Info(
			"health: run finished",
			"target", checker.target,
			"duration_ms", time.Since(startTime).Milliseconds(),
			"total_proxies", totalProxies,
			"checked", counters.checked.Load(),
			"success", counters.success.Load(),
			"failed", counters.failed.Load(),
			"removed", counters.removed.Load(),
		)
	}()

	proxies, err := checker.store.GetProxies(ctx)
	if err != nil {
		checker.logger.Warn("health: load proxies failed", "error", err)
		return
	}

	if len(proxies) == 0 {
		totalProxies = 0
		return
	}

	totalProxies = len(proxies)

	jobs := make(chan string)
	var waitGroup sync.WaitGroup

	workerCount := checker.concurrency
	if workerCount > len(proxies) {
		workerCount = len(proxies)
	}
	if workerCount <= 0 {
		workerCount = 1
	}

	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for proxyAddr := range jobs {
				checker.checkSingleProxy(ctx, proxyAddr, counters)
			}
		}()
	}

	for _, proxyAddr := range proxies {
		select {
		case <-ctx.Done():
			close(jobs)
			waitGroup.Wait()
			return
		case jobs <- proxyAddr:
		}
	}

	close(jobs)
	waitGroup.Wait()
}

func (checker *Checker) checkSingleProxy(ctx context.Context, proxyAddr string, counters *runCounters) {
	counters.checked.Add(1)

	if checker.tryDialThroughProxy(proxyAddr) {
		if err := checker.store.MarkProxySuccess(ctx, proxyAddr); err != nil {
			checker.logger.Warn("health: mark proxy success failed", "proxy", proxyAddr, "error", err)
		}
		counters.success.Add(1)
		return
	}

	counters.failed.Add(1)

	failures, err := checker.store.IncrementProxyFailure(ctx, proxyAddr)
	if err != nil {
		checker.logger.Warn("health: increment failure failed", "proxy", proxyAddr, "error", err)
		return
	}

	if failures < checker.maxFailures {
		return
	}

	if err := checker.store.RemoveProxyCascade(ctx, proxyAddr); err != nil {
		checker.logger.Warn("health: remove dead proxy failed", "proxy", proxyAddr, "error", err)
		return
	}

	counters.removed.Add(1)

	checker.logger.Info("health: proxy removed", "proxy", proxyAddr, "failures", failures)
}

func (checker *Checker) tryDialThroughProxy(proxyAddr string) bool {
	forward := deadlineDialer{
		timeout: checker.timeout,
		dialer: net.Dialer{
			Timeout: checker.timeout,
		},
	}

	socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, forward)
	if err != nil {
		return false
	}

	conn, err := socksDialer.Dial("tcp", checker.target)
	if err != nil {
		return false
	}
	defer conn.Close()

	host, port, err := net.SplitHostPort(checker.target)
	if err != nil {
		return false
	}

	if port != "443" {
		return true
	}

	_ = conn.SetDeadline(time.Now().Add(checker.timeout))
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		_ = tlsConn.Close()
		return false
	}
	_ = tlsConn.Close()
	return true
}

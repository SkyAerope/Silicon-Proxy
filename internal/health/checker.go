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
	defer checker.running.Store(false)

	proxies, err := checker.store.GetProxies(ctx)
	if err != nil {
		checker.logger.Warn("health: load proxies failed", "error", err)
		return
	}

	if len(proxies) == 0 {
		return
	}

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
				checker.checkSingleProxy(ctx, proxyAddr)
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

func (checker *Checker) checkSingleProxy(ctx context.Context, proxyAddr string) {
	if checker.tryDialThroughProxy(proxyAddr) {
		if err := checker.store.MarkProxySuccess(ctx, proxyAddr); err != nil {
			checker.logger.Warn("health: mark proxy success failed", "proxy", proxyAddr, "error", err)
		}
		return
	}

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

	checker.logger.Info("health: proxy removed", "proxy", proxyAddr, "failures", failures)
}

func (checker *Checker) tryDialThroughProxy(proxyAddr string) bool {
	dialer := &net.Dialer{Timeout: checker.timeout}
	socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, dialer)
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

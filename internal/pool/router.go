package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/silicon-proxy/Silicon-Proxy/internal/store"
)

type RoutingStore interface {
	GetProxyByAuthHash(ctx context.Context, authHash string) (string, error)
	IsProxyAlive(ctx context.Context, proxyAddr string) (bool, error)
	GetAliveProxiesByLoad(ctx context.Context) ([]store.ProxyStat, error)
	TryAssignAuthHashToProxy(ctx context.Context, authHash, proxyAddr string, maxBound int) (bool, error)
	UnassignAuthHashFromProxyIfMatch(ctx context.Context, authHash, proxyAddr string) (bool, error)
	IncrementProxyFailure(ctx context.Context, proxyAddr string) (int, error)
	RemoveProxyCascade(ctx context.Context, proxyAddr string) error
}

type AuthRouter struct {
	store           RoutingStore
	transportPool   *TransportPool
	maxAuthPerProxy int
	maxFailures     int
	cache           sync.Map
}

func NewAuthRouter(store RoutingStore, transportPool *TransportPool, maxAuthPerProxy, maxFailures int) *AuthRouter {
	return &AuthRouter{
		store:           store,
		transportPool:   transportPool,
		maxAuthPerProxy: maxAuthPerProxy,
		maxFailures:     maxFailures,
	}
}

func (router *AuthRouter) Resolve(ctx context.Context, authValue string) (*http.Transport, string, string, error) {
	authHash := hashAuthorization(authValue)

	if cached, ok := router.cache.Load(authHash); ok {
		proxyAddr := cached.(string)
		alive, err := router.store.IsProxyAlive(ctx, proxyAddr)
		if err == nil && alive {
			transport, transportErr := router.transportPool.Get(proxyAddr)
			if transportErr == nil {
				return transport, proxyAddr, authHash, nil
			}
		}
		router.cache.Delete(authHash)
	}

	assigned, err := router.store.GetProxyByAuthHash(ctx, authHash)
	if err != nil {
		return nil, "", authHash, err
	}

	if assigned != "" {
		alive, aliveErr := router.store.IsProxyAlive(ctx, assigned)
		if aliveErr == nil && alive {
			transport, transportErr := router.transportPool.Get(assigned)
			if transportErr == nil {
				router.cache.Store(authHash, assigned)
				return transport, assigned, authHash, nil
			}
		} else {
			_, _ = router.unbindAuthHash(ctx, authHash, assigned)
		}
	}

	stats, err := router.store.GetAliveProxiesByLoad(ctx)
	if err != nil {
		return nil, "", authHash, err
	}

	for _, proxyStat := range stats {
		if proxyStat.BoundCount >= router.maxAuthPerProxy {
			continue
		}

		ok, assignErr := router.store.TryAssignAuthHashToProxy(ctx, authHash, proxyStat.Addr, router.maxAuthPerProxy)
		if assignErr != nil {
			continue
		}
		if !ok {
			continue
		}

		transport, transportErr := router.transportPool.Get(proxyStat.Addr)
		if transportErr != nil {
			return nil, "", authHash, transportErr
		}

		router.cache.Store(authHash, proxyStat.Addr)
		return transport, proxyStat.Addr, authHash, nil
	}

	return nil, "", authHash, errors.New("no available proxy for auth")
}

func (router *AuthRouter) HandleProxyFailure(ctx context.Context, proxyAddr string) error {
	writeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	failures, err := router.store.IncrementProxyFailure(writeContext, proxyAddr)
	if err != nil {
		return err
	}

	if failures < router.maxFailures {
		return nil
	}

	if err := router.store.RemoveProxyCascade(writeContext, proxyAddr); err != nil {
		return err
	}

	router.evictProxyFromCache(proxyAddr)
	return nil
}

func (router *AuthRouter) UnbindAuthHash(ctx context.Context, authHash string, proxyAddr string) {
	_, _ = router.unbindAuthHash(ctx, authHash, proxyAddr)
}

func (router *AuthRouter) unbindAuthHash(ctx context.Context, authHash string, proxyAddr string) (bool, error) {
	writeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	unassigned, err := router.store.UnassignAuthHashFromProxyIfMatch(writeContext, authHash, proxyAddr)
	if err != nil {
		return false, err
	}
	if unassigned {
		router.cache.Delete(authHash)
	}
	return unassigned, nil
}

func (router *AuthRouter) evictProxyFromCache(proxyAddr string) {
	router.cache.Range(func(key, value any) bool {
		valueText, ok := value.(string)
		if !ok {
			return true
		}

		if valueText == proxyAddr {
			router.cache.Delete(key)
		}

		return true
	})
}

func hashAuthorization(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

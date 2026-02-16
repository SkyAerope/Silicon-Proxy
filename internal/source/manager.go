package source

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/silicon-proxy/Silicon-Proxy/internal/config"
)

type SourceStore interface {
	AddProxies(ctx context.Context, proxies []string) error
	GetSourceLastFetch(ctx context.Context, sourceID string) (time.Time, error)
	SetSourceLastFetch(ctx context.Context, sourceID string, value time.Time) error
}

type Manager struct {
	store      SourceStore
	sources    []Source
	logger     *slog.Logger
	afterFetch func(context.Context)
}

func NewManager(cfg *config.Config, store SourceStore, logger *slog.Logger, afterFetch func(context.Context)) (*Manager, error) {
	if store == nil {
		return nil, errors.New("source store is nil")
	}

	sources := make([]Source, 0, len(cfg.Sources))
	for _, sourceConfig := range cfg.Sources {
		switch sourceConfig.Type {
		case "url":
			sources = append(sources, NewURLSource(sourceConfig.URL, sourceConfig.IntervalDur, sourceConfig.WithPrefix))
		case "local":
			sources = append(sources, NewLocalSource(sourceConfig.Path, sourceConfig.IntervalDur, sourceConfig.WithPrefix))
		default:
			return nil, errors.New("unsupported source type")
		}
	}

	return &Manager{
		store:      store,
		sources:    sources,
		logger:     logger,
		afterFetch: afterFetch,
	}, nil
}

func (manager *Manager) Run(ctx context.Context) {
	for _, source := range manager.sources {
		current := source
		go manager.runSingleSource(ctx, current)
	}
}

func (manager *Manager) FetchDueNow(ctx context.Context) {
	for _, current := range manager.sources {
		manager.fetchIfDue(ctx, current)
	}
}

func (manager *Manager) runSingleSource(ctx context.Context, source Source) {
	manager.fetchIfDue(ctx, source)

	ticker := time.NewTicker(source.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.fetchIfDue(ctx, source)
		}
	}
}

func (manager *Manager) fetchIfDue(ctx context.Context, source Source) {
	lastFetch, err := manager.store.GetSourceLastFetch(ctx, source.ID())
	if err != nil {
		manager.logger.Warn("read source last fetch failed", "source", source.ID(), "error", err)
	}

	if !lastFetch.IsZero() && time.Since(lastFetch) < source.Interval() {
		return
	}

	proxies, err := source.Fetch()
	if err != nil {
		manager.logger.Warn("fetch source failed", "source", source.ID(), "error", err)
		return
	}

	if len(proxies) == 0 {
		manager.logger.Warn("source returned zero valid proxies", "source", source.ID())
		return
	}

	if err := manager.store.AddProxies(ctx, proxies); err != nil {
		manager.logger.Warn("store proxies failed", "source", source.ID(), "error", err)
		return
	}

	if manager.afterFetch != nil {
		manager.afterFetch(ctx)
	}

	if err := manager.store.SetSourceLastFetch(ctx, source.ID(), time.Now()); err != nil {
		manager.logger.Warn("store source last fetch failed", "source", source.ID(), "error", err)
	}

	manager.logger.Info("source fetched", "source", source.ID(), "proxy_count", len(proxies))
}

func (manager *Manager) FetchAllForce(ctx context.Context) {
	var waitGroup sync.WaitGroup

	for _, source := range manager.sources {
		waitGroup.Add(1)
		current := source
		go func() {
			defer waitGroup.Done()

			proxies, err := current.Fetch()
			if err != nil {
				manager.logger.Warn("force fetch source failed", "source", current.ID(), "error", err)
				return
			}

			if len(proxies) == 0 {
				return
			}

			if err := manager.store.AddProxies(ctx, proxies); err != nil {
				manager.logger.Warn("store force fetched proxies failed", "source", current.ID(), "error", err)
				return
			}

			if manager.afterFetch != nil {
				manager.afterFetch(ctx)
			}

			if err := manager.store.SetSourceLastFetch(ctx, current.ID(), time.Now()); err != nil {
				manager.logger.Warn("set last fetch after force failed", "source", current.ID(), "error", err)
			}
		}()
	}

	waitGroup.Wait()
}

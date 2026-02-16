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
	CountAliveProxies(ctx context.Context) (int, error)
}

type managedSource struct {
	source         Source
	maxLiveProxies int
}

type Manager struct {
	store      SourceStore
	scheduled  []managedSource
	onDemand   []managedSource
	logger     *slog.Logger
	afterFetch func(context.Context)
	onDemandCh chan struct{}
}

func NewManager(cfg *config.Config, store SourceStore, logger *slog.Logger, afterFetch func(context.Context)) (*Manager, error) {
	if store == nil {
		return nil, errors.New("source store is nil")
	}

	scheduled := make([]managedSource, 0, len(cfg.Sources))
	onDemand := make([]managedSource, 0, len(cfg.Sources))
	for _, sourceConfig := range cfg.Sources {
		maxLive := sourceConfig.MaxLiveProxies

		var built Source
		switch sourceConfig.Type {
		case "url":
			built = NewURLSource(sourceConfig.URL, sourceConfig.IntervalDur, sourceConfig.WithPrefix, sourceConfig.UseRegexVal)
		case "local":
			built = NewLocalSource(sourceConfig.Path, sourceConfig.IntervalDur, sourceConfig.WithPrefix, sourceConfig.UseRegexVal)
		default:
			return nil, errors.New("unsupported source type")
		}

		item := managedSource{source: built, maxLiveProxies: maxLive}
		if maxLive == -1 {
			onDemand = append(onDemand, item)
		} else {
			scheduled = append(scheduled, item)
		}
	}

	return &Manager{
		store:      store,
		scheduled:  scheduled,
		onDemand:   onDemand,
		logger:     logger,
		afterFetch: afterFetch,
		onDemandCh: make(chan struct{}, 1),
	}, nil
}

func (manager *Manager) NotifyNoAvailableProxy() {
	select {
	case manager.onDemandCh <- struct{}{}:
	default:
	}
}

func (manager *Manager) Run(ctx context.Context) {
	for _, item := range manager.scheduled {
		current := item
		go manager.runSingleSource(ctx, current)
	}

	if len(manager.onDemand) > 0 {
		go manager.runOnDemand(ctx)
	}
}

func (manager *Manager) FetchDueNow(ctx context.Context) {
	for _, current := range manager.scheduled {
		manager.fetchIfDue(ctx, current)
	}
}

func (manager *Manager) runSingleSource(ctx context.Context, item managedSource) {
	manager.fetchIfDue(ctx, item)

	ticker := time.NewTicker(item.source.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.fetchIfDue(ctx, item)
		}
	}
}

func (manager *Manager) runOnDemand(ctx context.Context) {
	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		select {
		case <-debounce.C:
		default:
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-manager.onDemandCh:
			// debounce bursts of "no available" signals
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(500 * time.Millisecond)
		case <-debounce.C:
			for _, item := range manager.onDemand {
				manager.fetchIfDue(ctx, item)
			}
		}
	}
}

func (manager *Manager) fetchIfDue(ctx context.Context, item managedSource) {
	source := item.source
	lastFetch, err := manager.store.GetSourceLastFetch(ctx, source.ID())
	if err != nil {
		manager.logger.Warn("read source last fetch failed", "source", source.ID(), "error", err)
	}

	if !lastFetch.IsZero() && time.Since(lastFetch) < source.Interval() {
		return
	}

	if item.maxLiveProxies > 0 {
		aliveCount, err := manager.store.CountAliveProxies(ctx)
		if err != nil {
			manager.logger.Warn("count alive proxies failed", "error", err)
		} else if aliveCount >= item.maxLiveProxies {
			manager.logger.Info("skip source fetch due to max_live_proxies", "source", source.ID(), "alive", aliveCount, "max_live_proxies", item.maxLiveProxies)
			return
		}
	}

	attemptedAt := time.Now()
	if err := manager.store.SetSourceLastFetch(ctx, source.ID(), attemptedAt); err != nil {
		manager.logger.Warn("store source last fetch failed", "source", source.ID(), "error", err)
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

	manager.logger.Info("source fetched", "source", source.ID(), "proxy_count", len(proxies))
}

func (manager *Manager) FetchAllForce(ctx context.Context) {
	var waitGroup sync.WaitGroup

	all := make([]managedSource, 0, len(manager.scheduled)+len(manager.onDemand))
	all = append(all, manager.scheduled...)
	all = append(all, manager.onDemand...)

	for _, item := range all {
		waitGroup.Add(1)
		current := item.source
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

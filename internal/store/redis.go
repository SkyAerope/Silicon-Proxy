package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	proxiesKeyPrefix         = "sp:proxies"
	deadProxiesKeyPrefix     = "sp:proxies:dead"
	sourceLastFetchKeyPrefix = "sp:source_last_fetch:"
	authKeyPrefix            = "sp:auth:"
	proxyMetaKeyPrefix       = "sp:proxy:"
	proxyAuthSetKeyPrefix    = "sp:proxy_auths:"
)

type ProxyStat struct {
	Addr       string
	BoundCount int
}

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(addr, password string, db int) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping redis failed: %w", err)
	}

	return &RedisStore{client: client}, nil
}

func (store *RedisStore) Close() error {
	return store.client.Close()
}

func (store *RedisStore) AddProxies(ctx context.Context, proxies []string) error {
	if len(proxies) == 0 {
		return nil
	}

	type seenCheck struct {
		proxyAddr string
		inLive    *redis.BoolCmd
		inDead    *redis.BoolCmd
	}

	checks := make([]seenCheck, 0, len(proxies))
	readPipe := store.client.Pipeline()
	for _, proxyAddr := range proxies {
		checks = append(checks, seenCheck{
			proxyAddr: proxyAddr,
			inLive:    readPipe.SIsMember(ctx, proxiesKeyPrefix, proxyAddr),
			inDead:    readPipe.SIsMember(ctx, deadProxiesKeyPrefix, proxyAddr),
		})
	}

	if _, err := readPipe.Exec(ctx); err != nil {
		return fmt.Errorf("check proxy dedupe failed: %w", err)
	}

	pipe := store.client.TxPipeline()
	for _, item := range checks {
		if item.inLive.Val() || item.inDead.Val() {
			continue
		}

		proxyAddr := item.proxyAddr
		pipe.SAdd(ctx, proxiesKeyPrefix, proxyAddr)
		metaKey := proxyMetaKey(proxyAddr)
		pipe.HSetNX(ctx, metaKey, "alive", 0)
		pipe.HSetNX(ctx, metaKey, "fail_count", 0)
		pipe.HSetNX(ctx, metaKey, "bound_count", 0)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("add proxies to redis failed: %w", err)
	}

	return nil
}

func (store *RedisStore) GetProxies(ctx context.Context) ([]string, error) {
	items, err := store.client.SMembers(ctx, proxiesKeyPrefix).Result()
	if err != nil {
		return nil, fmt.Errorf("read proxies set failed: %w", err)
	}
	return items, nil
}

func (store *RedisStore) SetSourceLastFetch(ctx context.Context, sourceID string, value time.Time) error {
	key := sourceLastFetchKey(sourceID)
	return store.client.Set(ctx, key, value.Unix(), 0).Err()
}

func (store *RedisStore) GetSourceLastFetch(ctx context.Context, sourceID string) (time.Time, error) {
	key := sourceLastFetchKey(sourceID)
	value, err := store.client.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}

	unixValue, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse source last fetch failed: %w", err)
	}

	return time.Unix(unixValue, 0), nil
}

func (store *RedisStore) GetProxyByAuthHash(ctx context.Context, authHash string) (string, error) {
	value, err := store.client.Get(ctx, authKey(authHash)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", nil
		}
		return "", fmt.Errorf("read auth mapping failed: %w", err)
	}

	return value, nil
}

func (store *RedisStore) IsProxyAlive(ctx context.Context, proxyAddr string) (bool, error) {
	value, err := store.client.HGet(ctx, proxyMetaKey(proxyAddr), "alive").Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("read proxy alive failed: %w", err)
	}

	return value == "1", nil
}

func (store *RedisStore) GetAliveProxiesByLoad(ctx context.Context) ([]ProxyStat, error) {
	allProxies, err := store.GetProxies(ctx)
	if err != nil {
		return nil, err
	}

	stats := make([]ProxyStat, 0, len(allProxies))
	for _, proxyAddr := range allProxies {
		meta, err := store.client.HMGet(ctx, proxyMetaKey(proxyAddr), "alive", "bound_count").Result()
		if err != nil {
			continue
		}

		if len(meta) < 2 {
			continue
		}

		aliveText := fmt.Sprintf("%v", meta[0])
		if aliveText != "1" {
			continue
		}

		boundCount, err := strconv.Atoi(fmt.Sprintf("%v", meta[1]))
		if err != nil {
			boundCount = 0
		}

		stats = append(stats, ProxyStat{
			Addr:       proxyAddr,
			BoundCount: boundCount,
		})
	}

	sort.Slice(stats, func(left, right int) bool {
		if stats[left].BoundCount == stats[right].BoundCount {
			return stats[left].Addr < stats[right].Addr
		}
		return stats[left].BoundCount < stats[right].BoundCount
	})

	return stats, nil
}

var assignScript = redis.NewScript(`
local auth_key = KEYS[1]
local proxy_meta_key = KEYS[2]
local proxy_auths_key = KEYS[3]
local auth_hash = ARGV[1]
local proxy_addr = ARGV[2]
local max_bound = tonumber(ARGV[3])

if redis.call("EXISTS", auth_key) == 1 then
  return 2
end

local alive = redis.call("HGET", proxy_meta_key, "alive")
if alive ~= "1" then
  return 0
end

local bound = tonumber(redis.call("HGET", proxy_meta_key, "bound_count") or "0")
if bound >= max_bound then
  return 0
end

redis.call("SET", auth_key, proxy_addr)
redis.call("SADD", proxy_auths_key, auth_hash)
redis.call("HINCRBY", proxy_meta_key, "bound_count", 1)
return 1
`)

func (store *RedisStore) TryAssignAuthHashToProxy(ctx context.Context, authHash, proxyAddr string, maxBound int) (bool, error) {
	result, err := assignScript.Run(ctx, store.client, []string{
		authKey(authHash),
		proxyMetaKey(proxyAddr),
		proxyAuthSetKey(proxyAddr),
	}, authHash, proxyAddr, maxBound).Int()
	if err != nil {
		return false, fmt.Errorf("run assign script failed: %w", err)
	}

	if result == 2 {
		return true, nil
	}

	return result == 1, nil
}

func (store *RedisStore) MarkProxySuccess(ctx context.Context, proxyAddr string) error {
	pipe := store.client.TxPipeline()
	pipe.SRem(ctx, deadProxiesKeyPrefix, proxyAddr)
	pipe.HSet(ctx, proxyMetaKey(proxyAddr), "alive", 1)
	pipe.HSet(ctx, proxyMetaKey(proxyAddr), "fail_count", 0)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("mark proxy success failed: %w", err)
	}
	return nil
}

func (store *RedisStore) IncrementProxyFailure(ctx context.Context, proxyAddr string) (int, error) {
	pipe := store.client.TxPipeline()
	failCountCmd := pipe.HIncrBy(ctx, proxyMetaKey(proxyAddr), "fail_count", 1)
	pipe.HSet(ctx, proxyMetaKey(proxyAddr), "alive", 0)
	pipe.SAdd(ctx, deadProxiesKeyPrefix, proxyAddr)

	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("increment proxy fail count failed: %w", err)
	}

	return int(failCountCmd.Val()), nil
}

func (store *RedisStore) RemoveProxyCascade(ctx context.Context, proxyAddr string) error {
	hashes, err := store.client.SMembers(ctx, proxyAuthSetKey(proxyAddr)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("read proxy auth set failed: %w", err)
	}

	pipe := store.client.TxPipeline()
	for _, authHash := range hashes {
		pipe.Del(ctx, authKey(authHash))
	}

	pipe.Del(ctx, proxyAuthSetKey(proxyAddr))
	pipe.Del(ctx, proxyMetaKey(proxyAddr))
	pipe.SRem(ctx, proxiesKeyPrefix, proxyAddr)
	pipe.SAdd(ctx, deadProxiesKeyPrefix, proxyAddr)

	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("remove proxy cascade failed: %w", err)
	}

	return nil
}

func sourceLastFetchKey(sourceID string) string {
	return sourceLastFetchKeyPrefix + sourceID
}

func authKey(authHash string) string {
	return authKeyPrefix + authHash
}

func proxyMetaKey(proxyAddr string) string {
	return proxyMetaKeyPrefix + proxyAddr
}

func proxyAuthSetKey(proxyAddr string) string {
	return proxyAuthSetKeyPrefix + proxyAddr
}

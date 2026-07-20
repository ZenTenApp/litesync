package internal

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/brave/go-sync/cache"
	lru "github.com/hashicorp/golang-lru"
)

const cacheSize = 1024

type FakeRedisClient struct {
	cache.RedisClient
	items *lru.Cache
	mu    sync.Mutex
}

func NewFakeRedisClient() *FakeRedisClient {
	c, _ := lru.New(cacheSize)
	return &FakeRedisClient{items: c}
}

func (c *FakeRedisClient) Set(ctx context.Context, key string, val string, ttl time.Duration) error {
	c.items.Add(key, val)
	return nil
}

func (c *FakeRedisClient) Get(ctx context.Context, key string, deleteAfterGet bool) (string, error) {
	value, ok := c.items.Get(key)
	if ok {
		if deleteAfterGet {
			c.items.Remove(key)
		}
		return value.(string), nil
	}
	return "", nil
}

func (c *FakeRedisClient) Del(ctx context.Context, keys ...string) error {
	for _, k := range keys {
		c.items.Remove(k)
	}
	return nil
}

func (c *FakeRedisClient) FlushAll(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items, _ = lru.New(cacheSize)
	return nil
}

// Incr atomically increments (or decrements) an integer counter stored at key.
// The value is stored as a decimal string, matching Redis INCR/DECR semantics.
func (c *FakeRedisClient) Incr(ctx context.Context, key string, subtract bool) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var current int
	if val, ok := c.items.Get(key); ok {
		n, err := strconv.Atoi(val.(string))
		if err != nil {
			return 0, err
		}
		current = n
	}

	if subtract {
		current--
	} else {
		current++
	}

	c.items.Add(key, strconv.Itoa(current))
	return current, nil
}

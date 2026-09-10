package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis 仅作为查询缓存(cache-aside),绝不在借用/归还的控制路径上。
// Redis 不可用时所有操作静默降级为直查 MySQL。

const overviewCacheKey = "licensehub:overview:v1"
const overviewCacheTTL = 30 * time.Second

type Cache struct {
	rdb *redis.Client // nil 表示缓存禁用
}

func NewCache(addr string, db int) *Cache {
	if addr == "" {
		return &Cache{}
	}
	return &Cache{rdb: redis.NewClient(&redis.Options{Addr: addr, DB: db})}
}

func (c *Cache) Enabled() bool { return c.rdb != nil }

// GetOverview 命中返回 true
func (c *Cache) GetOverview(ctx context.Context) (*Overview, bool) {
	if c.rdb == nil {
		return nil, false
	}
	data, err := c.rdb.Get(ctx, overviewCacheKey).Bytes()
	if err != nil {
		return nil, false
	}
	var ov Overview
	if err := json.Unmarshal(data, &ov); err != nil {
		return nil, false
	}
	return &ov, true
}

func (c *Cache) SetOverview(ctx context.Context, ov *Overview) {
	if c.rdb == nil {
		return
	}
	data, err := json.Marshal(ov)
	if err != nil {
		return
	}
	if err := c.rdb.Set(ctx, overviewCacheKey, data, overviewCacheTTL).Err(); err != nil {
		log.Printf("cache set: %v", err)
	}
}

// Invalidate 任何写操作(借用/归还/回收/调额)后调用
func (c *Cache) Invalidate(ctx context.Context) {
	if c.rdb == nil {
		return
	}
	if err := c.rdb.Del(ctx, overviewCacheKey).Err(); err != nil {
		log.Printf("cache invalidate: %v", err)
	}
}

func (c *Cache) Close() {
	if c.rdb != nil {
		c.rdb.Close()
	}
}

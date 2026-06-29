package db

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"pa-ai/api-gateway/src/config"
)

// RateLimiter membungkus Redis untuk counter rate limit per kontak.
type RateLimiter struct {
	rdb    *redis.Client
	max    int
	window time.Duration
}

// NewRateLimiter membuka koneksi Redis dan menyiapkan limiter.
func NewRateLimiter(ctx context.Context, cfg config.Config) (*RateLimiter, error) {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &RateLimiter{rdb: rdb, max: cfg.RateLimitMax, window: cfg.RateLimitWindow}, nil
}

// Close menutup koneksi Redis.
func (r *RateLimiter) Close() { _ = r.rdb.Close() }

// Allow menaikkan counter untuk `phone` dan mengembalikan (diizinkan, count, error).
// Counter di-set TTL = window saat pertama dibuat.
func (r *RateLimiter) Allow(ctx context.Context, phone string) (bool, int64, error) {
	key := "ratelimit:" + phone
	count, err := r.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, 0, err
	}
	if count == 1 {
		// key baru → pasang TTL window.
		if err := r.rdb.Expire(ctx, key, r.window).Err(); err != nil {
			return false, count, err
		}
	}
	return count <= int64(r.max), count, nil
}

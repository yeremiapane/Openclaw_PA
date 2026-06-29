package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/model"
)

// Cache adalah Layer 2 memory (Redis) untuk percakapan aktif — buffer cepat
// recent messages + state. TTL 48 jam; sliding window 40 pesan (Fase 7).
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

// ConvTTL = masa hidup buffer percakapan di Redis (48 jam, sesuai dokumen).
const ConvTTL = 48 * time.Hour

// ConvWindow = jumlah maksimum pesan yang disimpan di buffer Redis.
const ConvWindow = 40

// NewCache membuka koneksi Redis terpisah untuk buffer percakapan.
func NewCache(ctx context.Context, cfg config.Config) (*Cache, error) {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &Cache{rdb: rdb, ttl: ConvTTL}, nil
}

// Close menutup koneksi Redis.
func (c *Cache) Close() { _ = c.rdb.Close() }

func msgsKey(convID string) string  { return "conv:" + convID + ":msgs" }
func stateKey(convID string) string { return "conv:" + convID + ":state" }

// GetMessages mengembalikan buffer pesan dari Redis. found=false bila cache miss.
func (c *Cache) GetMessages(ctx context.Context, convID string) (msgs []model.Message, found bool, err error) {
	raw, err := c.rdb.Get(ctx, msgsKey(convID)).Result()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, false, err
	}
	return msgs, true, nil
}

// SetMessages menulis buffer pesan ke Redis dengan TTL (warm-up cache).
func (c *Cache) SetMessages(ctx context.Context, convID string, msgs []model.Message) error {
	b, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, msgsKey(convID), b, c.ttl).Err()
}

// AppendMessages menambah pesan ke buffer, memangkas ke sliding window, refresh TTL.
func (c *Cache) AppendMessages(ctx context.Context, convID string, newMsgs ...model.Message) error {
	existing, _, err := c.GetMessages(ctx, convID)
	if err != nil {
		return err
	}
	existing = append(existing, newMsgs...)
	if len(existing) > ConvWindow {
		existing = existing[len(existing)-ConvWindow:]
	}
	return c.SetMessages(ctx, convID, existing)
}

// GetState mengembalikan state dari Redis. found=false bila cache miss.
func (c *Cache) GetState(ctx context.Context, convID string) (state string, found bool, err error) {
	state, err = c.rdb.Get(ctx, stateKey(convID)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return state, true, nil
}

// SetState menulis state ke Redis dengan TTL.
func (c *Cache) SetState(ctx context.Context, convID, state string) error {
	return c.rdb.Set(ctx, stateKey(convID), state, c.ttl).Err()
}

func epochKey(convID string) string { return "conv:" + convID + ":ocepoch" }

// GetEpoch mengembalikan epoch sesi OpenClaw untuk percakapan (0 bila belum ada).
// Epoch dinaikkan saat sesi OpenClaw terdeteksi terkontaminasi (lihat BumpEpoch),
// sehingga session-key efektif berubah dan riwayat lama dibuang.
func (c *Cache) GetEpoch(ctx context.Context, convID string) int {
	v, err := c.rdb.Get(ctx, epochKey(convID)).Int()
	if err != nil {
		return 0
	}
	return v
}

// BumpEpoch menaikkan epoch sesi OpenClaw (reset sesi) dan mengembalikan nilai baru.
func (c *Cache) BumpEpoch(ctx context.Context, convID string) (int, error) {
	v, err := c.rdb.Incr(ctx, epochKey(convID)).Result()
	if err != nil {
		return 0, err
	}
	_ = c.rdb.Expire(ctx, epochKey(convID), c.ttl).Err()
	return int(v), nil
}

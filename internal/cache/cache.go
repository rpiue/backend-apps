// Package cache reemplaza los objetos en memoria del código JS
// (cacheDatosApp{}, cacheUsuarios{}, userSecrets{} y el registry Map de FCM)
// por Redis, de modo que el estado sobrevive reinicios y escala horizontalmente.
package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	rdb *redis.Client
}

func New(redisURL string) (*Cache, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Cache{rdb: rdb}, nil
}

func (c *Cache) Client() *redis.Client { return c.rdb }

// GetJSON deserializa el valor en dst. Devuelve (false, nil) si no existe.
func (c *Cache) GetJSON(ctx context.Context, key string, dst any) (bool, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(val), dst)
}

// SetJSON serializa v y lo guarda con TTL (0 = sin expiración).
func (c *Cache) SetJSON(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, b, ttl).Err()
}

func (c *Cache) Del(ctx context.Context, keys ...string) error {
	return c.rdb.Del(ctx, keys...).Err()
}

// SetNX fija la clave solo si no existe (dedupe atómico). Devuelve true si la
// fijó (no existía), false si ya existía. Útil para idempotencia de webhooks.
func (c *Cache) SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, val, ttl).Result()
}

// --- Conjuntos: reemplazan el registry Map(email -> Set(tokens)) de notifications.js ---

func (c *Cache) SAdd(ctx context.Context, key string, members ...string) error {
	args := make([]any, len(members))
	for i, m := range members {
		args[i] = m
	}
	return c.rdb.SAdd(ctx, key, args...).Err()
}

func (c *Cache) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *Cache) SRem(ctx context.Context, key string, members ...string) error {
	args := make([]any, len(members))
	for i, m := range members {
		args[i] = m
	}
	return c.rdb.SRem(ctx, key, args...).Err()
}

func (c *Cache) Close() error { return c.rdb.Close() }

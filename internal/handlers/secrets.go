package handlers

import (
	"context"
	"time"
)

// Reemplaza cacheUser.js (objeto en memoria) por Redis. Guarda solo email/pin
// por (app, key). TTL amplio; es solo una caché para acelerar validaciones.
type userSecret struct {
	Email string `json:"email"`
	Pin   string `json:"pin"`
}

func secretKey(app, key string) string { return "secret:" + app + ":" + key }

func (h *Handler) saveUserSecret(ctx context.Context, app, key, email, pin string) {
	_ = h.Cache.SetJSON(ctx, secretKey(app, key), userSecret{Email: email, Pin: pin}, 24*time.Hour)
}

func (h *Handler) getUserSecret(ctx context.Context, app, key string) (userSecret, bool) {
	var s userSecret
	ok, err := h.Cache.GetJSON(ctx, secretKey(app, key), &s)
	if err != nil || !ok {
		return userSecret{}, false
	}
	return s, true
}

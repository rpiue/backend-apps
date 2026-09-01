package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"codex/backend/internal/ai"
	"codex/backend/internal/auth"
	"codex/backend/internal/btcpay"
	"codex/backend/internal/cache"
	"codex/backend/internal/chat"
	"codex/backend/internal/config"
	"codex/backend/internal/firebase"
	"codex/backend/internal/hub"
	"codex/backend/internal/mercadopago"
	"codex/backend/internal/middleware"
	"codex/backend/internal/notify"
	"codex/backend/internal/resources"
	"codex/backend/internal/store"
)

// Handler agrupa las dependencias compartidas por todos los endpoints.
type Handler struct {
	Cfg       *config.Config
	Cache     *cache.Cache
	Store     *store.Store
	Notifier  *notify.Notifier
	FB        *firebase.Client
	Auth      *auth.Auth
	Resources *resources.Store
	MP        *mercadopago.Client
	Hub       *hub.Hub
	AI        *ai.Client
	Debounce  *ai.Debouncer
	BTC       *btcpay.Client
	// Chat se asigna tras construir el Handler (bridge de token chat-admin).
	Chat *chat.Service
	// Limiter (rate limiting Redis) se asigna tras construir el Handler.
	Limiter *middleware.Limiter
}

func New(cfg *config.Config, c *cache.Cache, s *store.Store, n *notify.Notifier, fb *firebase.Client, a *auth.Auth, res *resources.Store, mp *mercadopago.Client, hb *hub.Hub, aic *ai.Client, deb *ai.Debouncer, btc *btcpay.Client) *Handler {
	return &Handler{Cfg: cfg, Cache: c, Store: s, Notifier: n, FB: fb, Auth: a, Resources: res, MP: mp, Hub: hb, AI: aic, Debounce: deb, BTC: btc}
}

// rl devuelve un middleware de rate limit por minuto para la ruta `name`.
// Si no hay Limiter configurado (tests), es un passthrough.
func (h *Handler) rl(name string, perMin int) func(http.Handler) http.Handler {
	if h.Limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return h.Limiter.Limit(name, perMin, time.Minute)
}

// writeJSON responde JSON con el status indicado.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// readJSON decodifica el body en dst. Un body VACÍO se trata como válido (dst
// queda con sus valores por defecto): varios clientes hacen POST con
// Content-Type: application/json pero sin cuerpo (p.ej. /datosApp), y eso no es
// un error — el handler aplica sus defaults. Solo un JSON realmente malformado
// responde 400. Devuelve false (y ya respondió 400) si el JSON es inválido.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true // body vacío: sin campos, se usan los valores por defecto
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return false
	}
	return true
}

// notImplemented es un marcador honesto para rutas aún no portadas (fases siguientes).
func notImplemented(w http.ResponseWriter, fase string) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "ruta aún no portada a Go",
		"fase":  fase,
	})
}

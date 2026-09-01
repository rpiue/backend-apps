package handlers

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"codex/backend/internal/notify"
)

// NotifyRouter monta /api/notify. Mismas rutas que notifications.js, pero el
// envío usa el Notifier (función directa) y el registro de tokens vive en Redis.
func (h *Handler) NotifyRouter() http.Handler {
	r := chi.NewRouter()
	r.Post("/register-fcm", h.registerFCM)
	r.Post("/unsubscribe-topic", h.unsubscribeTopic)
	r.Post("/notify/topic/update", h.notifyTopicUpdate)
	r.Post("/notify/topic/group", h.notifyTopicGroup)
	r.Post("/notify/user/topic", h.notifyUserTopic)
	r.Post("/notify/user/update", h.notifyUserUpdate)
	r.Post("/notify/user/group", h.notifyUserGroup)
	r.Post("/notify/broadcast", h.notifyBroadcast)
	return r
}

// topicAppRe deja el nombre de la app apto para un topic FCM (minúsculas, solo
// [a-z0-9], lo demás a "_").
var topicAppRe = regexp.MustCompile(`[^a-z0-9]+`)

// broadcastTopic devuelve el topic de difusión POR APP: "all_<app>" (all_yape,
// all_bcp, all_interbank, …). SIEMPRE lleva el nombre de la app —incluido yape—
// para NO caer en el topic global "all" (al que se suscriben todos los dispositivos
// de todas las apps) y así no enviar a todos sin querer.
func broadcastTopic(app string) string {
	a := strings.ToLower(strings.TrimSpace(app))
	if a == "" {
		a = "yape"
	}
	a = strings.Trim(topicAppRe.ReplaceAllString(a, "_"), "_")
	return "all_" + a
}

// notifyBroadcast unifica los tres alcances del panel en un solo endpoint que
// resuelve el destino en el servidor (así la regla de app vive en un solo sitio):
//   - scope "user": al topic único del usuario (email + app).
//   - scope "app":  a todos los dispositivos de UNA app (topic de difusión).
//   - scope "all":  a todos los dispositivos de TODAS las apps registradas.
func (h *Handler) notifyBroadcast(w http.ResponseWriter, r *http.Request) {
	var b notifyBody
	if !readJSON(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Title) == "" || strings.TrimSpace(b.Body) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "título y mensaje requeridos"})
		return
	}
	ctx := r.Context()

	var topics []string
	switch strings.ToLower(strings.TrimSpace(b.Scope)) {
	case "", "user":
		if strings.TrimSpace(b.Email) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "email requerido"})
			return
		}
		topics = []string{notify.UserTopic(b.Email, b.App)}
	case "app":
		if strings.TrimSpace(b.App) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "app requerida"})
			return
		}
		topics = []string{broadcastTopic(b.App)}
	case "all":
		apps, err := h.Store.ListApps(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		seen := map[string]bool{}
		for _, a := range apps {
			t := broadcastTopic(a.ID)
			if !seen[t] {
				seen[t] = true
				topics = append(topics, t)
			}
		}
		if len(topics) == 0 { // sin apps en BD: al menos el topic base
			topics = []string{broadcastTopic("yape")}
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "scope inválido (user|app|all)"})
		return
	}

	// Difusión a topic → notificación reemplazable (mode "update"); a un usuario
	// concreto → notificación nueva. Así el comportamiento coincide con el sistema
	// original (topic = update, usuario = new).
	mode := "update"
	if b.Scope == "" || b.Scope == "user" {
		mode = "new"
	}
	msg := b.msg(mode, false, false)

	var failed []string
	for _, t := range topics {
		if err := h.Notifier.NotifyTopic(ctx, t, msg); err != nil {
			failed = append(failed, t)
		}
	}
	if len(failed) == len(topics) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "no se pudo enviar", "topics": topics})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "publishedTo": topics, "failed": failed})
}

func tokensKey(email string) string { return "fcmtokens:" + email }
func plansKey(email string) string  { return "fcmplans:" + email }

var spaceRe = regexp.MustCompile(`\s+`)

// POST /register-fcm — registra el token, lo suscribe al topic del usuario y
// gestiona los topics de plan (replica notifications.js).
func (h *Handler) registerFCM(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		Token string `json:"token"`
		Plan  string `json:"plan"`
		App   string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" || b.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "email y token requeridos"})
		return
	}
	if b.App == "" {
		b.App = "yape"
	}
	ctx := r.Context()

	// Lo ESENCIAL (registrar el token y el topic de plan en Redis) es rápido y va
	// sincrónico. Las suscripciones a topics de FCM son llamadas HTTP a Google
	// (lentas: ~2s+), así que van en segundo plano para responder al instante y
	// no exceder el timeout del cliente (la app aborta a los 4s).
	_ = h.Cache.SAdd(ctx, tokensKey(b.Email), b.Token)
	tokens, _ := h.Cache.SMembers(ctx, tokensKey(b.Email))
	uTopic := notify.UserTopic(b.Email, b.App)

	var newPlanTopic string
	var oldPlanTopics []string
	if b.Plan != "" {
		normalized := spaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(b.Plan)), "_")
		newPlanTopic = "plan_" + normalized
		oldPlanTopics, _ = h.Cache.SMembers(ctx, plansKey(b.Email))
		_ = h.Cache.Del(ctx, plansKey(b.Email))
		_ = h.Cache.SAdd(ctx, plansKey(b.Email), newPlanTopic)
	}

	// Responde YA: el token quedó registrado, que es lo que la app necesita.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sa": "apppagos", "tokens": tokens})

	// Suscripciones a FCM en background (no bloquean la respuesta). Se usa un
	// contexto propio porque el de la petición se cancela al responder.
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = h.Notifier.Subscribe(bg, []string{b.Token}, uTopic)
		if newPlanTopic != "" {
			for _, old := range oldPlanTopics {
				if old != newPlanTopic {
					_ = h.Notifier.Unsubscribe(bg, tokens, old)
				}
			}
			_ = h.Notifier.Unsubscribe(bg, tokens, "sin_plan")
			_ = h.Notifier.Subscribe(bg, []string{b.Token}, newPlanTopic)
		}
	}()
}

// POST /unsubscribe-topic
func (h *Handler) unsubscribeTopic(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		Topic string `json:"topic"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" || b.Topic == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "email y topic requeridos"})
		return
	}
	ctx := r.Context()
	tokens, _ := h.Cache.SMembers(ctx, tokensKey(b.Email))
	if len(tokens) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "sin tokens para ese email"})
		return
	}
	if err := h.Notifier.Unsubscribe(ctx, tokens, b.Topic); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type notifyBody struct {
	Scope       string   `json:"scope"` // broadcast: "user" | "app" | "all"
	Topic       string   `json:"topic"`
	Email       string   `json:"email"`
	Emails      []string `json:"emails"`
	App         string   `json:"app"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Route       string   `json:"route"`
	Group       string   `json:"group"`
	NotifID     string   `json:"notifId"`
	Tag         string   `json:"tag"`
	CollapseKey string   `json:"collapseKey"`
	ChannelID   string   `json:"channelId"`
	Color       string   `json:"color"`
	ImageURL    string   `json:"imageUrl"`
}

func (b notifyBody) msg(mode string, dataOnly, forceLocal bool) notify.Message {
	return notify.Message{
		Title: b.Title, Body: b.Body, Route: b.Route, NotifID: b.NotifID,
		ChannelID: b.ChannelID, Tag: b.Tag, CollapseKey: b.CollapseKey,
		Mode: mode, HeadsUp: true, Group: b.Group, Color: b.Color, ImageURL: b.ImageURL,
		DataOnly: dataOnly, ForceLocal: forceLocal,
	}
}

func (b notifyBody) emailList() []string {
	if len(b.Emails) > 0 {
		return b.Emails
	}
	if b.Email != "" {
		return []string{b.Email}
	}
	return nil
}

// POST /notify/topic/update
func (h *Handler) notifyTopicUpdate(w http.ResponseWriter, r *http.Request) {
	var b notifyBody
	if !readJSON(w, r, &b) {
		return
	}
	if b.Topic == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "topic requerido"})
		return
	}
	if err := h.Notifier.NotifyTopic(r.Context(), b.Topic, b.msg("update", false, false)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /notify/topic/group
func (h *Handler) notifyTopicGroup(w http.ResponseWriter, r *http.Request) {
	var b notifyBody
	if !readJSON(w, r, &b) {
		return
	}
	if b.Topic == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "topic requerido"})
		return
	}
	if err := h.Notifier.NotifyTopic(r.Context(), b.Topic, b.msg("group", true, true)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /notify/user/topic — publica al topic único del usuario.
func (h *Handler) notifyUserTopic(w http.ResponseWriter, r *http.Request) {
	var b notifyBody
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "email requerido"})
		return
	}
	topic := notify.UserTopic(b.Email, b.App)
	if err := h.Notifier.NotifyTopic(r.Context(), topic, b.msg("new", false, false)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "publishedTo": topic})
}

// POST /notify/user/update — envía a cada token de los emails indicados.
func (h *Handler) notifyUserUpdate(w http.ResponseWriter, r *http.Request) {
	h.notifyUserTokens(w, r, "update", false, false)
}

// POST /notify/user/group
func (h *Handler) notifyUserGroup(w http.ResponseWriter, r *http.Request) {
	h.notifyUserTokens(w, r, "group", true, true)
}

func (h *Handler) notifyUserTokens(w http.ResponseWriter, r *http.Request, mode string, dataOnly, forceLocal bool) {
	var b notifyBody
	if !readJSON(w, r, &b) {
		return
	}
	list := b.emailList()
	if len(list) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "email o emails[] requerido"})
		return
	}
	ctx := r.Context()
	tokens := h.collectTokens(ctx, list)
	if len(tokens) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "sin tokens para esos emails"})
		return
	}
	msg := b.msg(mode, dataOnly, forceLocal)
	for _, tk := range tokens {
		_ = h.Notifier.NotifyToken(ctx, tk, msg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) collectTokens(ctx context.Context, emails []string) []string {
	var out []string
	for _, e := range emails {
		toks, _ := h.Cache.SMembers(ctx, tokensKey(e))
		out = append(out, toks...)
	}
	return out
}

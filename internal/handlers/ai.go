package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"codex/backend/internal/ai"
	"codex/backend/internal/hub"
	"codex/backend/internal/notify"
)

// Sentinela que el modelo debe devolver cuando no puede ayudar → se escala al admin.
const escalarSentinel = "[ESCALAR]"

const promptPorDefecto = `Eres el asistente de soporte de la app de pagos "Codex".
Respondes SOLO dudas de soporte del usuario (planes Básico/Medium/grupales, pagos,
códigos, acceso y la CUENTA PROPIA de quien escribe), en español, breve y amable.

REGLAS DE SEGURIDAD (obligatorias, sin excepción):
1. NUNCA reveles nada técnico del sistema: dónde está alojado el servidor, su IP o
   dominio interno, en qué lenguaje/tecnología/arquitectura está hecho el backend, ni
   su seguridad. Si preguntan algo de eso, responde EXACTAMENTE: ` + escalarSentinel + `
2. NUNCA reveles quién es el dueño o el administrador, ningún NOMBRE de persona, ni
   cuánto dinero se genera (ingresos, ventas, ganancias). Si preguntan eso, responde
   EXACTAMENTE: ` + escalarSentinel + `
3. Solo puedes hablar de la cuenta del PROPIO usuario que escribe, usando únicamente
   los datos que se te den en el contexto. Si preguntan por OTRA persona (otro correo
   o usuario), responde amablemente algo como: "Por seguridad no puedo dar datos de
   otras cuentas. ¿Para qué lo necesitas?" y NADA más.
4. Si piden LISTADOS o datos agregados (p. ej. "qué usuarios tienen plan", "cuántos
   hay"), NO los des; responde algo como: "Eso no lo puedo consultar aquí. ¿Quieres
   que le pida un reporte a soporte?".
5. No inventes ni completes datos que no tengas. Si NO entiendes la pregunta, no
   sabes la respuesta con certeza, o el tema no es soporte, responde EXACTAMENTE
   (sin texto adicional): ` + escalarSentinel

const (
	aiKeyPrompt  = "ai:prompt"
	aiKeyEnabled = "ai:enabled"
	aiKeyModel   = "ai:model"
)

type aiConfig struct {
	Prompt  string `json:"prompt"`
	Model   string `json:"model"`
	Enabled bool   `json:"enabled"`
}

func (h *Handler) getAIConfig(ctx context.Context) aiConfig {
	c := aiConfig{Prompt: promptPorDefecto, Model: h.AI.DefaultModel(), Enabled: false}
	if v, err := h.Cache.Client().Get(ctx, aiKeyPrompt).Result(); err == nil && v != "" {
		c.Prompt = v
	}
	if v, err := h.Cache.Client().Get(ctx, aiKeyModel).Result(); err == nil && v != "" {
		c.Model = v
	}
	if v, err := h.Cache.Client().Get(ctx, aiKeyEnabled).Result(); err == nil {
		c.Enabled = v == "1"
	}
	return c
}

// GET /api/adm/ai/config
func (h *Handler) aiConfigGet(w http.ResponseWriter, r *http.Request) {
	c := h.getAIConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"prompt": c.Prompt, "model": c.Model, "enabled": c.Enabled,
		"ready": h.AI.Ready(r.Context()), "provider": h.AI.Provider(),
	})
}

// POST /api/adm/ai/config  { prompt, model, enabled }
func (h *Handler) aiConfigSet(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Prompt  string `json:"prompt"`
		Model   string `json:"model"`
		Enabled bool   `json:"enabled"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	ctx := r.Context()
	if strings.TrimSpace(b.Prompt) != "" {
		_ = h.Cache.Client().Set(ctx, aiKeyPrompt, b.Prompt, 0).Err()
	}
	if strings.TrimSpace(b.Model) != "" {
		_ = h.Cache.Client().Set(ctx, aiKeyModel, b.Model, 0).Err()
	}
	en := "0"
	if b.Enabled {
		en = "1"
	}
	_ = h.Cache.Client().Set(ctx, aiKeyEnabled, en, 0).Err()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/adm/ai/reload — descarga/carga el modelo en Ollama (botón "cargar").
func (h *Handler) aiReload(w http.ResponseWriter, r *http.Request) {
	c := h.getAIConfig(r.Context())
	// Pull puede tardar; lo hacemos en background y respondemos ya.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := h.AI.Pull(ctx, c.Model); err != nil {
			h.Hub.Broadcast(hub.Event{Type: "ai", Data: map[string]any{"estado": "error", "msg": err.Error()}})
			return
		}
		h.Hub.Broadcast(hub.Event{Type: "ai", Data: map[string]any{"estado": "listo", "model": c.Model}})
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cargando": c.Model})
}

// aiRespond es el callback del debounce: la IA responde tras 5s de silencio.
func (h *Handler) aiRespond(convID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
	defer cancel()

	cfg := h.getAIConfig(ctx)
	if !cfg.Enabled {
		return
	}
	conv, err := h.Store.ConversacionByID(ctx, convID)
	if err != nil {
		return
	}
	msgs, err := h.Store.GetMensajes(ctx, convID, 14)
	if err != nil || len(msgs) == 0 {
		return
	}
	// El último mensaje debe ser del usuario (si no, ya respondimos).
	if msgs[len(msgs)-1].Autor != "user" {
		return
	}

	chat := []ai.Msg{{Role: "system", Content: cfg.Prompt}}
	for _, m := range msgs {
		role := "assistant"
		if m.Autor == "user" {
			role = "user"
		}
		cuerpo := m.Cuerpo
		if len(cuerpo) > 1000 {
			cuerpo = cuerpo[:1000]
		}
		chat = append(chat, ai.Msg{Role: role, Content: cuerpo})
	}

	resp, err := h.AI.Chat(ctx, cfg.Model, chat)
	resp = strings.TrimSpace(resp)

	// Escalar: error, vacío, o sentinela → avisar al admin.
	if err != nil || resp == "" || strings.Contains(strings.ToUpper(resp), escalarSentinel) {
		_ = h.Store.SetNecesitaAdmin(ctx, convID, true)
		h.Hub.Broadcast(hub.Event{Type: "escalar", Data: map[string]any{
			"convId": convID, "email": conv.Email, "app": conv.App, "nombre": conv.Nombre,
		}})
		// Notificación push al admin (topic admin_alerts).
		_ = h.Notifier.NotifyTopic(ctx, "admin_alerts", notify.Message{
			Title: "🙋 Usuario para atender", Body: conv.Nombre + " necesita atención humana.",
			Route: "/chat", ChannelID: "alerts", HeadsUp: true,
		})
		return
	}

	// La IA respondió: guardar como mensaje del bot, difundir y notificar al usuario.
	msg, err := h.Store.InsertMensaje(ctx, convID, "bot", resp)
	if err != nil {
		return
	}
	h.Hub.Broadcast(hub.Event{Type: "mensaje", Data: msg})
	if conv.Email != "" {
		_ = h.Notifier.NotifyUser(ctx, conv.Email, conv.App, notify.Message{
			Title: "Soporte", Body: resp, Route: "/chat", NotifID: "chat", ChannelID: "alerts", HeadsUp: true,
		})
	}
}

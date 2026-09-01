package handlers

import (
	"net/http"

	"codex/backend/internal/hub"
	"codex/backend/internal/notify"
)

// chatToken emite un token de chat-admin para el panel ya autenticado, evitando
// un segundo login para el chat avanzado (/api/chat).
func (h *Handler) chatToken(w http.ResponseWriter, r *http.Request) {
	if h.Chat == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "chat no disponible"})
		return
	}
	token, user, err := h.Chat.IssueAdminToken(r)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "user": user})
}

func (h *Handler) chatConversaciones(w http.ResponseWriter, r *http.Request) {
	convs, err := h.Store.ListConversaciones(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, convs)
}

func (h *Handler) chatMensajes(w http.ResponseWriter, r *http.Request) {
	conv := r.URL.Query().Get("conv")
	if conv == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conv requerido"})
		return
	}
	msgs, err := h.Store.GetMensajes(r.Context(), conv, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// chatEnviar: el admin envía un mensaje al usuario. Se guarda, se difunde al
// panel y se NOTIFICA al usuario por función directa (regla -{app} incluida).
func (h *Handler) chatEnviar(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ConvID string `json:"convId"`
		Email  string `json:"email"`
		App    string `json:"app"`
		Nombre string `json:"nombre"`
		Cuerpo string `json:"cuerpo"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Cuerpo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cuerpo requerido"})
		return
	}
	ctx := r.Context()

	convID := b.ConvID
	email, app := b.Email, b.App
	if convID == "" {
		if email == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "convId o email requerido"})
			return
		}
		id, err := h.Store.EnsureConversacion(ctx, email, app, b.Nombre)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		convID = id
	} else {
		c, err := h.Store.ConversacionByID(ctx, convID)
		if err == nil {
			email, app = c.Email, c.App
		}
	}

	msg, err := h.Store.InsertMensaje(ctx, convID, "admin", b.Cuerpo)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// El admin está atendiendo: quita el flag de "necesita atención".
	_ = h.Store.SetNecesitaAdmin(ctx, convID, false)

	// Notificación al usuario (push). UserTopic aplica el sufijo -{app}.
	if email != "" {
		_ = h.Notifier.NotifyUser(ctx, email, app, notify.Message{
			Title: "Tienes un mensaje nuevo", Body: b.Cuerpo, Route: "/chat",
			NotifID: "chat", ChannelID: "alerts", HeadsUp: true,
		})
	}

	h.Hub.Broadcast(hub.Event{Type: "mensaje", Data: msg})
	writeJSON(w, http.StatusOK, msg)
}

func (h *Handler) chatLeidos(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ConvID string `json:"convId"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if err := h.Store.MarcarLeidos(r.Context(), b.ConvID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// chatIncoming: inyecta un mensaje entrante del usuario (lo usará la integración
// que reciba mensajes del lado del usuario). Difunde al panel en tiempo real.
func (h *Handler) chatIncoming(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email  string `json:"email"`
		App    string `json:"app"`
		Nombre string `json:"nombre"`
		Cuerpo string `json:"cuerpo"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" || b.Cuerpo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email y cuerpo requeridos"})
		return
	}
	ctx := r.Context()
	convID, err := h.Store.EnsureConversacion(ctx, b.Email, b.App, b.Nombre)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	msg, err := h.Store.InsertMensaje(ctx, convID, "user", b.Cuerpo)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.Hub.Broadcast(hub.Event{Type: "mensaje", Data: msg})

	// Debounce: esperar 5s de silencio antes de que la IA responda. Si el usuario
	// vuelve a escribir, este Trigger reinicia el contador.
	h.Debounce.Trigger(convID, func() { h.aiRespond(convID) })

	writeJSON(w, http.StatusOK, msg)
}

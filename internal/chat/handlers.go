package chat

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

type ctxKey string

const claimsKey ctxKey = "chatClaims"

// Router devuelve el router montado en /api/chat (replica chat/index.js).
func (s *Service) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/ws", s.rt.HandleWS)
	r.Get("/theme/{app}", s.theme)
	r.Get("/theme", s.theme)

	r.Post("/session", s.postSession)
	r.Post("/guest/session", s.postGuestSession)
	r.Post("/support/message", s.postSupportMessage)
	r.Post("/support/attachments", s.postSupportAttachments)
	r.Post("/admin/session", s.postAdminSession)

	// Rutas con sesión de chat (Bearer).
	r.Group(func(pr chi.Router) {
		pr.Use(s.requireChatAuth)
		pr.Post("/ws-ticket", s.postWsTicket)
		pr.Post("/logout", s.postLogout)
		pr.Get("/messages", s.getMessages)
		pr.Post("/messages", s.postMessages)
		pr.Post("/messages/{messageId}/reactions", s.postReaction)
		pr.Delete("/messages/{messageId}/reactions", s.deleteReaction)
		pr.Post("/presence", s.postPresence)
		pr.Get("/online/{conversationId}", s.getOnline)
		pr.Post("/attachments", s.postAttachments)
		pr.Get("/attachments/{id}", s.getAttachmentFile)
	})

	// Rutas admin (Bearer + role admin).
	r.Group(func(pr chi.Router) {
		pr.Use(s.requireAdmin)
		pr.Get("/admin/conversations", s.getAdminConversations)
		pr.Get("/admin/conversations/{conversationId}/user-info", s.getUserInfo)
		pr.Post("/admin/conversations/{conversationId}/grant-device-access", s.grantDeviceAccess)
		pr.Delete("/admin/conversations/{conversationId}", s.deleteConversation)
		pr.Delete("/admin/conversations", s.deleteAllConversations)
		pr.Get("/admin/users/lookup", s.adminUsersLookup)
		pr.Post("/admin/conversations/ensure", s.adminEnsureConversation)
		pr.Get("/admin/automations", s.getAutomations)
		pr.Post("/admin/automations", s.saveAutomationH)
		pr.Post("/admin/automations/media", s.postAutomationMedia)
		pr.Get("/admin/automations/{id}/media", s.getAutomationMedia)
		pr.Delete("/admin/automations/{id}", s.deleteAutomationH)
		pr.Get("/admin/notification-settings", s.getNotificationSettings)
		pr.Post("/admin/notification-settings", s.saveNotificationSettings)
		pr.Post("/admin/password", s.changePassword)
		pr.Post("/admin/grant-plan", s.grantPlan)
		pr.Get("/admin/malware", s.getMalwareStats)
		pr.Get("/admin/labels", s.getLabels)
		pr.Post("/admin/labels", s.createLabel)
		pr.Put("/admin/labels/{id}", s.updateLabel)
		pr.Delete("/admin/labels/{id}", s.deleteLabel)
		pr.Post("/admin/conversations/{conversationId}/labels", s.assignConversationLabel)
		pr.Delete("/admin/conversations/{conversationId}/labels/{labelId}", s.unassignConversationLabel)
		pr.Get("/admin/labels/{id}/users", s.getLabelUsers)
		pr.Get("/admin/reminders", s.getReminders)
		pr.Post("/admin/reminders", s.createReminder)
		pr.Put("/admin/reminders/{id}", s.updateReminder)
		pr.Delete("/admin/reminders/{id}", s.deleteReminder)
		pr.Post("/admin/reminders/{id}/send-now", s.sendReminderNow)
	})

	return r
}

// --- Middleware ---

func (s *Service) requireChatAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("Authorization")
		raw = strings.TrimPrefix(raw, "Bearer ")
		if raw == "" {
			writeErr(w, http.StatusUnauthorized, "No autorizado")
			return
		}
		claims, err := s.parseToken(raw)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Sesion invalida")
			return
		}
		fp := s.fingerprintFromRequest(r)
		ok, err := s.db.validateChatSession(r.Context(), claims.JTI, claims.Sub, claims.Role, fp.userAgentHash, fp.ipHash)
		if err != nil || !ok {
			writeErr(w, http.StatusUnauthorized, "Sesion invalida")
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) requireAdmin(next http.Handler) http.Handler {
	return s.requireChatAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := claimsOf(r)
		if c.Role != "admin" {
			writeErr(w, http.StatusForbidden, "Solo admin")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func claimsOf(r *http.Request) *chatClaims {
	c, _ := r.Context().Value(claimsKey).(*chatClaims)
	return c
}

// --- Rutas públicas ---

func (s *Service) theme(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	if app == "" {
		app = r.URL.Query().Get("app")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "theme": themeForApp(app)})
}

func (s *Service) postSession(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App      string `json:"app"`
		Email    string `json:"email"`
		Pin      string `json:"pin"`
		Clave    string `json:"clave"`
		DeviceID string `json:"deviceId"`
		// snake_case por compatibilidad con la app móvil (acepta ambos).
		DeviceIDSnake string `json:"device_id"`
	}
	if !readBody(w, r, &b) {
		return
	}
	pin := b.Pin
	if pin == "" {
		pin = b.Clave
	}
	dev := b.DeviceID
	if dev == "" {
		dev = b.DeviceIDSnake
	}
	// Freno de fuerza bruta ANTES de comprobar el PIN (ver authlimit.go).
	if !s.permitirIntentoAuth(w, r, "session", b.Email) {
		return
	}
	auth, err := s.authenticateUserSession(r.Context(), b.App, b.Email, pin)
	if err != nil {
		// Se cuenta el FALLO (no el intento): a quien entra bien no se le gasta
		// cupo, y el contador refleja lo que interesa vigilar.
		s.registrarFalloAuth(r, "session", b.Email)
		writeErr(w, http.StatusUnauthorized, "Email o pin incorrecto")
		return
	}
	s.limpiarIntentosAuth(r, "session", b.Email)
	token, err := s.signSessionFor(s.fingerprintFromRequest(r), s.clientIP(r), auth.user.ID, "user", auth.conversation.App, &auth.conversation.ID, auth.user.Email, normalizeDeviceID(dev))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "token": token, "user": auth.user, "conversation": auth.conversation, "theme": themeForApp(auth.nameApp),
	})
}

func (s *Service) postGuestSession(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App           string `json:"app"`
		DeviceID      string `json:"deviceId"`
		DeviceIDSnake string `json:"device_id"`
	}
	if !readBody(w, r, &b) {
		return
	}
	app := normalizeApp(b.App)
	dev := b.DeviceID
	if dev == "" {
		dev = b.DeviceIDSnake
	}
	device := normalizeDeviceID(dev)
	if device == nil {
		writeErr(w, http.StatusBadRequest, "deviceId requerido")
		return
	}
	// No hay credencial que adivinar aquí, pero cada llamada CREA un usuario
	// invitado y una conversación: sin freno, es una forma trivial de llenar la
	// base de datos con basura.
	if !s.permitirIntentoAuth(w, r, "guest", "") {
		return
	}
	user, err := s.db.ensureGuestUserByIp(r.Context(), app, sha256Hex(s.clientIP(r)))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	conv, err := s.db.ensureConversation(r.Context(), app, user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	token, err := s.signSessionFor(s.fingerprintFromRequest(r), s.clientIP(r), user.ID, "user", conv.App, &conv.ID, user.Email, device)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "token": token, "user": user, "conversation": conv, "theme": themeForApp(app),
	})
}

func (s *Service) postAdminSession(w http.ResponseWriter, r *http.Request) {
	var b struct{ Email, Password string }
	if !readBody(w, r, &b) {
		return
	}
	email := normalizeEmail(b.Email)
	if email == "" || b.Password == "" {
		writeErr(w, http.StatusBadRequest, "Faltan email y password")
		return
	}
	// El login de admin es el objetivo más valioso del sistema y estaba sin
	// ningún freno propio: solo lo cubría el límite global de 120/min por IP,
	// que para probar contraseñas es prácticamente barra libre.
	if !s.permitirIntentoAuth(w, r, "admin", email) {
		return
	}
	admin, err := s.db.verifyAdmin(r.Context(), email, b.Password)
	if err != nil || admin == nil {
		s.registrarFalloAuth(r, "admin", email)
		writeErr(w, http.StatusUnauthorized, "Credenciales invalidas")
		return
	}
	s.limpiarIntentosAuth(r, "admin", email)
	token, err := s.signSessionFor(s.fingerprintFromRequest(r), s.clientIP(r), admin.ID, "admin", admin.App, nil, admin.Email, nil)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "Credenciales invalidas")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "user": admin})
}

func (s *Service) postWsTicket(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	ticket, expiresIn, err := s.db.createWsTicket(r.Context(), c.JTI, 60)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ticket": ticket, "expiresIn": expiresIn})
}

func (s *Service) postLogout(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	_ = s.db.revokeChatSession(r.Context(), c.JTI)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Mensajes ---

func (s *Service) getMessages(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	var adminVal int64
	if v := r.URL.Query().Get("conversationId"); v != "" {
		adminVal, _ = strconv.ParseInt(v, 10, 64)
	}
	conversationID := convID(c, adminVal)
	if conversationID == 0 {
		writeErr(w, http.StatusBadRequest, "conversationId requerido")
		return
	}
	if c.Role != "admin" {
		ok, _ := s.db.getConversationMember(r.Context(), conversationID, c.Sub)
		if !ok {
			writeErr(w, http.StatusForbidden, "Sin permiso")
			return
		}
	}
	var afterID, beforeID *int64
	if v := r.URL.Query().Get("afterId"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		afterID = &n
	}
	if v := r.URL.Query().Get("beforeId"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		beforeID = &n
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	messages, err := s.db.listMessages(r.Context(), conversationID, afterID, beforeID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	if len(messages) > 0 {
		last := messages[len(messages)-1].ID
		_, _ = s.db.markConversationPresence(r.Context(), conversationID, c.Role, &last)
	} else {
		_, _ = s.db.markConversationPresence(r.Context(), conversationID, c.Role, nil)
	}
	presence := s.rt.presencePayload(r.Context(), conversationID)
	var firstID, lastID *int64
	if len(messages) > 0 {
		firstID = &messages[0].ID
		lastID = &messages[len(messages)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "messages": messages,
		"page": map[string]any{
			"limit": limit, "count": len(messages), "firstId": firstID, "lastId": lastID,
			"hasMoreBefore": afterID == nil && len(messages) == limit,
			"hasMoreAfter":  afterID != nil && len(messages) == limit,
		},
		"presence": presence,
	})
}

func (s *Service) postMessages(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	var b messageBody
	if !readBody(w, r, &b) {
		return
	}
	conversationID := convID(c, b.ConversationID)
	if conversationID == 0 {
		writeErr(w, http.StatusBadRequest, "conversationId requerido")
		return
	}
	if c.Role == "admin" {
		conv, _ := s.db.getConversation(r.Context(), conversationID)
		if conv == nil || conv.AdminID != c.Sub {
			writeErr(w, http.StatusForbidden, "Sin permiso")
			return
		}
	} else {
		ok, _ := s.db.getConversationMember(r.Context(), conversationID, c.Sub)
		if !ok {
			writeErr(w, http.StatusForbidden, "Sin permiso")
			return
		}
	}
	enc, err := parseEncrypted(b.Encrypted, b.textValue())
	if err != nil {
		writeChatErr(w, err)
		return
	}
	if enc == nil {
		writeErr(w, http.StatusBadRequest, "Mensaje invalido")
		return
	}
	fp, sig, err := s.enforceUserSendGuard(r.Context(), c.Role, conversationID, c.Sub, b.textValue(), nil, nil)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	created, err := s.db.createMessage(r.Context(), conversationID, c.Sub, "text", *enc, b.ClientNonce, fp, sig, b.replyToID())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	before := created.ID - 1
	msgs, _ := s.db.listMessages(r.Context(), conversationID, &before, nil, 1)
	_, _ = s.db.markConversationPresence(r.Context(), conversationID, c.Role, &created.ID)
	s.notifyUserIfOffline(conversationID, c.Role)
	if len(msgs) > 0 {
		s.rt.broadcastConversation(conversationID, map[string]any{"type": "message", "conversationId": conversationID, "message": msgs[0]})
	}
	s.rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": conversationID})
	if c.Role == "user" {
		s.notifyAdminIfOffline(conversationID, "Un usuario envio un mensaje.")
		if conv, _ := s.db.getConversation(r.Context(), conversationID); conv != nil {
			s.notifyPlanLeadIfAdminOffline(conversationID, conv.AppName, b.textValue())
			// Respuesta rápida primero; si NINGUNA coincide, pasa a la IA (batching).
			if !s.maybeAutoReply(r.Context(), conversationID, conv.AppName, b.textValue()) {
				s.aiEnqueue(conversationID, b.textValue())
			}
		}
	}
	var out any
	if len(msgs) > 0 {
		out = msgs[0]
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "message": out})
}

// convID replica conversationIdFromRequest: admin usa el valor provisto
// (query/body/param), el usuario usa el de su token.
func convID(c *chatClaims, adminValue int64) int64 {
	if c.Role == "admin" {
		return adminValue
	}
	if c.ConversationID != nil {
		return *c.ConversationID
	}
	return 0
}

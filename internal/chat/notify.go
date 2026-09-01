package chat

import (
	"context"
	"strconv"
	"time"
)

// cancelPendingOfflineUserNotification se mantiene por compatibilidad con el
// handler de "usuario conectado". Ya no hay timers pendientes (el push es
// instantáneo), pero limpia cualquier timer residual por seguridad.
func (s *Service) cancelPendingOfflineUserNotification(conversationID int64) {
	s.pendingOfflineMu.Lock()
	defer s.pendingOfflineMu.Unlock()
	if t, ok := s.pendingOffline[conversationID]; ok {
		t.Stop()
		delete(s.pendingOffline, conversationID)
	}
}

func offlineUserDedupeKey(conversationID int64) string {
	return "user-offline:" + strconv.FormatInt(conversationID, 10)
}

// notifyUserIfOffline envía un push AL INSTANTE al usuario cuando el soporte
// (admin) escribe y el usuario NO está viendo el chat. El texto lleva el número
// de mensajes nuevos ("Tienes N mensajes nuevos") y usa un tag por conversación
// para que la notificación se REEMPLACE (no se apile) al llegar más mensajes.
func (s *Service) notifyUserIfOffline(conversationID int64, senderRole string) {
	if senderRole != "admin" {
		return
	}
	ctx := context.Background()
	conv, _ := s.db.getConversationDetails(ctx, conversationID)
	if conv == nil || conv.UserEmail == "" {
		return
	}
	// Si el usuario está viendo el chat en vivo, no hace falta push.
	if s.rt.isConversationUserOnline(conversationID) {
		return
	}
	if conv.LastUserPresenceAt != nil && conv.LastUserPresenceAt.After(time.Now().Add(-15*time.Second)) {
		return
	}
	n, _ := s.db.countUnseenAdminMessages(ctx, conversationID)
	if n < 1 {
		n = 1
	}
	body := "Tienes " + strconv.Itoa(n) + " mensajes nuevos"
	if n == 1 {
		body = "Tienes 1 mensaje nuevo"
	}
	tag := "chat-" + strconv.FormatInt(conversationID, 10)
	_ = s.push.PushTagged(ctx, conv.UserEmail, conv.AppName, "Soporte te respondió",
		body, "/chat?app="+conv.AppName, tag)
}

func (s *Service) notifyAdminIfOffline(conversationID int64, message string) {
	if s.rt.isAnyAdminOnline() {
		return
	}
	ctx := context.Background()
	conv, _ := s.db.getConversationDetails(ctx, conversationID)
	if conv == nil || conv.AdminEmail == "" {
		return
	}
	can, _ := s.db.shouldSendNotification(ctx, "admin:"+strconv.FormatInt(conversationID, 10), 10080)
	if !can {
		return
	}
	if message == "" {
		message = "Hay un nuevo mensaje esperando respuesta."
	}
	_ = s.push.Push(ctx, conv.AdminEmail, "yape", "Usuarios necesitan atencion", message, "/admin/chat")
}

func appLabel(app string) string {
	switch normalizeApp(app) {
	case "bcp":
		return "BCP Fake"
	case "interbank":
		return "Interbank Fake"
	}
	return "Yape Fake"
}

func (s *Service) notifyPlanLeadIfAdminOffline(conversationID int64, app, text string) {
	if s.rt.isAnyAdminOnline() || !looksLikePlanPurchase(text) {
		return
	}
	ctx := context.Background()
	email, _ := s.db.getAdminNotificationEmail(ctx, app, s.cfg.AdminNotificationMail)
	if email == "" {
		return
	}
	can, _ := s.db.shouldSendNotification(ctx, "admin-plan:"+strconv.FormatInt(conversationID, 10), 15)
	if !can {
		return
	}
	_ = s.push.Push(ctx, email, app, appLabel(app)+" - Compra de Plan",
		"Hay un cliente interesado en comprar un plan.", "/admin/chat")
}

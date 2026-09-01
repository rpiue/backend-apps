package chat

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Service) getAdminConversations(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	limit := 40
	if v, _ := strconv.Atoi(qp.Get("limit")); v > 0 {
		limit = v
	}
	if limit > 100 {
		limit = 100
	}
	offset, _ := strconv.Atoi(qp.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	labelID, _ := strconv.ParseInt(qp.Get("labelId"), 10, 64)
	app := qp.Get("app")
	convs, err := s.db.listConversationsForAdmin(r.Context(), ConvFilters{
		App: app, Search: qp.Get("q"),
		UnreadOnly: qp.Get("unreadOnly") == "1" || qp.Get("unreadOnly") == "true",
		LabelID:    labelID, Limit: limit, Offset: offset,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	unreadTotal, _ := s.db.countUnreadConversations(r.Context(), app)
	ids := make([]int64, len(convs))
	for i := range convs {
		ids[i] = convs[i].ID
	}
	labelsByConv, _ := s.db.labelsForConversationIDs(r.Context(), ids)
	for i := range convs {
		o := s.rt.getConversationOnline(convs[i].ID)
		convs[i].Online = &o
		labs := labelsByConv[convs[i].ID]
		if labs == nil {
			labs = []Label{}
		}
		convs[i].Labels = labs
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "conversations": convs,
		"page": map[string]any{
			"limit": limit, "offset": offset, "count": len(convs),
			"nextOffset": offset + len(convs), "hasMore": len(convs) == limit,
		},
		"unreadTotal": unreadTotal,
		"adminOnline": s.rt.isAnyAdminOnline(),
	})
}

func (s *Service) getUserInfo(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	conversationID, _ := strconv.ParseInt(chiParam(r, "conversationId"), 10, 64)
	conv, _ := s.db.getConversationDetails(r.Context(), conversationID)
	if conv == nil || conv.AdminID != c.Sub {
		writeErr(w, http.StatusNotFound, "Conversacion no encontrada")
		return
	}
	session, _ := s.db.getLatestUserSessionInfo(r.Context(), conv.UserID)
	var sessOut any
	if session != nil {
		active := session.RevokedAt == nil && session.ExpiresAt.After(time.Now())
		sessOut = map[string]any{
			"ip": session.IPAddress, "deviceId": session.DeviceID, "lastSeenAt": session.LastSeenAt,
			"createdAt": session.CreatedAt, "expiresAt": session.ExpiresAt, "active": active,
		}
	}
	stats, _ := s.db.getUserDeviceStats(r.Context(), conv.UserID)
	var statsOut any
	if stats != nil {
		statsOut = map[string]any{
			"deviceCount": stats.DeviceCount, "sessionCount": stats.SessionCount,
			"activeSessions": stats.ActiveSessions, "firstSeenAt": stats.FirstSeenAt,
			"lastSeenAt": stats.LastSeenAt,
		}
	}
	isGuest := conv.UserGuestNumber != nil
	// Los planes solo aplican a usuarios con cuenta (los invitados no reciben plan).
	var plans []PlanOption
	var accountOut any
	if !isGuest {
		plans, _ = s.dir.Plans(r.Context(), conv.AppName)
		// Datos de la cuenta en Firebase: plan actual, vencimiento, estado y nº de
		// compras. Best-effort: si Firebase falla, el resto de la ficha igual carga.
		if users, _, err := s.dir.Lookup(r.Context(), conv.AppName, conv.UserEmail, nil); err == nil && len(users) > 0 {
			u := users[0]
			accountOut = map[string]any{
				"plan": u.Plan, "fechaFinal": u.FechaFinal, "acceso": u.Acceso, "compras": u.Compras,
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"user": map[string]any{
			"id": conv.UserID, "email": conv.UserEmail, "nombre": conv.UserNombre, "apellidos": conv.UserApellidos,
			"guestNumber": conv.UserGuestNumber, "isGuest": isGuest,
		},
		"conversation": map[string]any{
			"id": conv.ID, "app": conv.AppName, "status": conv.Status,
			"createdAt": conv.CreatedAt, "lastUserPresenceAt": conv.LastUserPresenceAt,
		},
		"session": sessOut,
		"stats":   statsOut,
		"plans":   plans,
		"account": accountOut,
	})
}

func (s *Service) grantDeviceAccess(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	conversationID, _ := strconv.ParseInt(chiParam(r, "conversationId"), 10, 64)
	conv, _ := s.db.getConversationDetails(r.Context(), conversationID)
	if conv == nil || conv.AdminID != c.Sub {
		writeErr(w, http.StatusNotFound, "Conversacion no encontrada")
		return
	}
	var b struct {
		DeviceID  string `json:"deviceId"`
		DeviceID2 string `json:"device_id"`
	}
	_ = readBodyOptional(r, &b)
	dev := b.DeviceID
	if dev == "" {
		dev = b.DeviceID2
	}
	if dev == "" {
		if sess, _ := s.db.getLatestUserSessionInfo(r.Context(), conv.UserID); sess != nil && sess.DeviceID != nil {
			dev = *sess.DeviceID
		}
	}
	deviceID := normalizeDeviceID(dev)
	if deviceID == nil {
		writeErr(w, http.StatusBadRequest, "El usuario aun no envio deviceId desde el frontend.")
		return
	}
	ok, message, err := s.dir.GrantDevice(r.Context(), conv.AppName, conv.UserEmail, *deviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	if ok {
		_ = s.push.Push(r.Context(), conv.UserEmail, conv.AppName, "Dispositivo autorizado",
			"Cierra la app y vuelve a abrirla para aplicar el nuevo acceso.", "")
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"ok": ok, "message": message, "deviceId": *deviceID})
}

func (s *Service) deleteConversation(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	conversationID, _ := strconv.ParseInt(chiParam(r, "conversationId"), 10, 64)
	conv, _ := s.db.getConversation(r.Context(), conversationID)
	if conv == nil || conv.AdminID != c.Sub {
		writeErr(w, http.StatusNotFound, "Conversacion no encontrada")
		return
	}
	if _, err := s.db.deleteConversationHard(r.Context(), conversationID); err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	s.rt.deleteConversationFromRealtime(conversationID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) deleteAllConversations(w http.ResponseWriter, r *http.Request) {
	ids, err := s.db.deleteAllConversationsHard(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	for _, id := range ids {
		s.rt.deleteConversationFromRealtime(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": len(ids)})
}

func (s *Service) adminUsersLookup(w http.ResponseWriter, r *http.Request) {
	q := normalizeEmail(r.URL.Query().Get("q"))
	if len(q) < 5 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": []any{}})
		return
	}
	users := s.findFirebaseUsersByEmail(r.Context(), r.URL.Query().Get("app"), q)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": users})
}

func (s *Service) adminEnsureConversation(w http.ResponseWriter, r *http.Request) {
	var b struct{ App, Email string }
	if !readBody(w, r, &b) {
		return
	}
	email := normalizeEmail(b.Email)
	if email == "" {
		writeErr(w, http.StatusBadRequest, "Email requerido")
		return
	}
	users := s.findFirebaseUsersByEmail(r.Context(), b.App, email)
	if len(users) == 0 {
		writeErr(w, http.StatusNotFound, "Usuario no encontrado en Firebase")
		return
	}
	fb := users[0]
	user, err := s.db.upsertChatUser(r.Context(), fb.App, fb.Email, &fb.ID, fb.Nombre, fb.Apellidos, fb.Telefono, "user")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	conv, err := s.db.ensureConversation(r.Context(), fb.App, user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	online := s.rt.getConversationOnline(conv.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "conversation": map[string]any{
		"id": conv.ID, "app": conv.App, "status": "open", "updatedAt": time.Now(),
		"lastMessage": nil, "unreadCount": 0, "online": online,
		"user": map[string]any{"id": user.ID, "email": user.Email, "nombre": user.Nombre, "apellidos": user.Apellidos, "telefono": user.Telefono},
	}})
}

// --- Automations ---

func (s *Service) getAutomations(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.listAutomations(r.Context(), r.URL.Query().Get("app"), false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

func (s *Service) saveAutomationH(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID               *int64                `json:"id"`
		Title            string                `json:"title"`
		Patterns         []string              `json:"patterns"`
		Response         string                `json:"response"`
		App              *string               `json:"app"`
		Enabled          *bool                 `json:"enabled"`
		SendPaymentQR    *bool                 `json:"sendPaymentQr"`
		SendQRSnake      *bool                 `json:"send_payment_qr"`
		MinScore         *float64              `json:"minScore"`
		MinScoreSnake    *float64              `json:"min_score"`
		Attachment       *AutomationAttachment `json:"attachment"`
		RemoveAttachment *bool                 `json:"removeAttachment"`
	}
	if !readBody(w, r, &b) {
		return
	}
	enabled := b.Enabled == nil || *b.Enabled
	sendQR := (b.SendPaymentQR != nil && *b.SendPaymentQR) || (b.SendQRSnake != nil && *b.SendQRSnake)
	minScore := 0.45
	if b.MinScore != nil {
		minScore = *b.MinScore
	} else if b.MinScoreSnake != nil {
		minScore = *b.MinScoreSnake
	}
	// Media: se persiste lo que envía el panel (siempre manda el attachment actual).
	// removeAttachment lo limpia. Se valida que la ruta esté dentro del UploadDir.
	att := b.Attachment
	if b.RemoveAttachment != nil && *b.RemoveAttachment {
		att = nil
	}
	if att != nil && !s.validAutomationMediaPath(att.Path) {
		writeErr(w, http.StatusBadRequest, "Adjunto inválido")
		return
	}
	item, err := s.db.saveAutomation(r.Context(), b.ID, b.Title, b.Patterns, b.Response, b.App, enabled, sendQR, minScore, att)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": item})
}

func (s *Service) deleteAutomationH(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	_ = s.db.deleteAutomation(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Notification settings ---

// getReminders lista los recordatorios.
func (s *Service) getReminders(w http.ResponseWriter, r *http.Request) {
	rems, err := s.db.listReminders(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reminders": rems})
}

// createReminder crea un recordatorio.
func (s *Service) createReminder(w http.ResponseWriter, r *http.Request) {
	var b Reminder
	if !readBody(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Title) == "" || strings.TrimSpace(b.Body) == "" ||
		b.StartDate == "" || b.EndDate == "" {
		writeErr(w, http.StatusBadRequest, "title, body, startDate y endDate son requeridos")
		return
	}
	rem, err := s.db.createReminder(r.Context(), b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo crear el recordatorio")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "reminder": rem})
}

// updateReminder actualiza un recordatorio.
func (s *Service) updateReminder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	var b Reminder
	if !readBody(w, r, &b) {
		return
	}
	b.ID = id
	if id == 0 {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if err := s.db.updateReminder(r.Context(), b); err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo actualizar")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteReminder elimina un recordatorio.
func (s *Service) deleteReminder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	if id == 0 {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if err := s.db.deleteReminder(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo eliminar")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sendReminderNow dispara un recordatorio de inmediato (envío manual), sin
// esperar a su horario. Útil para "enviar ahora a los que deben".
func (s *Service) sendReminderNow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	rems, err := s.db.listReminders(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	var rem *Reminder
	for i := range rems {
		if rems[i].ID == id {
			rem = &rems[i]
			break
		}
	}
	if rem == nil {
		writeErr(w, http.StatusNotFound, "Recordatorio no encontrado")
		return
	}
	if rem.LabelID == nil {
		writeErr(w, http.StatusBadRequest, "El recordatorio no tiene etiqueta objetivo")
		return
	}
	targets, err := s.db.reminderTargets(r.Context(), *rem.LabelID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudieron resolver destinatarios")
		return
	}
	slotKey := "manual:" + strconv.FormatInt(time.Now().Unix(), 10)
	_, _ = s.db.claimReminderSlot(r.Context(), rem.ID, slotKey, len(targets))
	s.sendReminderTo(r.Context(), *rem, targets)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": len(targets)})
}

// getLabelUsers devuelve los usuarios de las conversaciones con una etiqueta
// (para previsualizar a quién se le enviará, ej. "los que deben").
func (s *Service) getLabelUsers(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	users, err := s.db.labelUsers(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": users})
}

// getLabels lista todas las etiquetas.
func (s *Service) getLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.db.listLabels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "labels": labels})
}

// createLabel crea una etiqueta { name, color }.
func (s *Service) createLabel(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Name) == "" {
		writeErr(w, http.StatusBadRequest, "Nombre requerido")
		return
	}
	label, err := s.db.createLabel(r.Context(), b.Name, strings.TrimSpace(b.Color))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo crear la etiqueta")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "label": label})
}

// updateLabel actualiza nombre/color y el flag notify de una etiqueta.
func (s *Service) updateLabel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	if id == 0 {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var b struct {
		Name   string `json:"name"`
		Color  string `json:"color"`
		Notify bool   `json:"notify"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Name) == "" {
		writeErr(w, http.StatusBadRequest, "Nombre requerido")
		return
	}
	color := strings.TrimSpace(b.Color)
	if color == "" {
		color = "#008069"
	}
	label, err := s.db.updateLabel(r.Context(), id, b.Name, color, b.Notify)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo actualizar la etiqueta")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": label})
}

// deleteLabel elimina una etiqueta por id.
func (s *Service) deleteLabel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	if id == 0 {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if err := s.db.deleteLabel(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo eliminar")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// assignConversationLabel asigna una etiqueta a una conversación.
func (s *Service) assignConversationLabel(w http.ResponseWriter, r *http.Request) {
	conversationID, _ := strconv.ParseInt(chiParam(r, "conversationId"), 10, 64)
	var b struct {
		LabelID int64 `json:"labelId"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if conversationID == 0 || b.LabelID == 0 {
		writeErr(w, http.StatusBadRequest, "conversationId y labelId requeridos")
		return
	}
	if err := s.db.assignLabel(r.Context(), conversationID, b.LabelID); err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo asignar")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// unassignConversationLabel quita una etiqueta de una conversación.
func (s *Service) unassignConversationLabel(w http.ResponseWriter, r *http.Request) {
	conversationID, _ := strconv.ParseInt(chiParam(r, "conversationId"), 10, 64)
	labelID, _ := strconv.ParseInt(chiParam(r, "labelId"), 10, 64)
	if conversationID == 0 || labelID == 0 {
		writeErr(w, http.StatusBadRequest, "ids requeridos")
		return
	}
	if err := s.db.unassignLabel(r.Context(), conversationID, labelID); err != nil {
		writeErr(w, http.StatusInternalServerError, "No se pudo quitar")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// getMalwareStats devuelve la analítica de malware para el panel admin.
func (s *Service) getMalwareStats(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	stats, err := s.db.malwareStats(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stats": stats})
}

func (s *Service) getNotificationSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.listAdminNotificationSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	byApp := map[string]string{}
	for _, row := range rows {
		byApp[row.AppName] = row.Email
	}
	def := byApp["default"]
	if def == "" {
		def = s.cfg.AdminNotificationMail
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": map[string]any{
		"defaultEmail": def, "yapeEmail": byApp["yape"], "bcpEmail": byApp["bcp"], "interbankEmail": byApp["interbank"],
	}})
}

func (s *Service) saveNotificationSettings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		DefaultEmail, YapeEmail, BcpEmail, InterbankEmail string
	}
	if !readBody(w, r, &b) {
		return
	}
	rows, err := s.db.saveAdminNotificationSettings(r.Context(), b.DefaultEmail, b.BcpEmail, b.YapeEmail, b.InterbankEmail)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": rows})
}

func (s *Service) changePassword(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	var b struct {
		CurrentPassword, NewPassword string
	}
	if !readBody(w, r, &b) {
		return
	}
	if len(b.NewPassword) < 10 {
		writeErr(w, http.StatusBadRequest, "La nueva contraseña debe tener al menos 10 caracteres")
		return
	}
	ok, err := s.db.updateAdminPassword(r.Context(), c.Sub, b.CurrentPassword, b.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Contraseña actual incorrecta")
		return
	}
	_ = s.db.revokeChatSession(r.Context(), c.JTI)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) grantPlan(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	var b struct {
		ConversationID int64  `json:"conversationId"`
		Plan           string `json:"plan"`
	}
	if !readBody(w, r, &b) {
		return
	}
	plan := strings.TrimSpace(b.Plan)
	allowed := map[string]bool{
		"Basico": true, "Medium": true, "Premium": true,
		"Basico Grupal": true, "Medium Grupal": true, "Medium Grupal Plus": true,
	}
	if b.ConversationID == 0 || !allowed[plan] {
		writeErr(w, http.StatusBadRequest, "Plan invalido")
		return
	}
	conv, _ := s.db.getConversationDetails(r.Context(), b.ConversationID)
	if conv == nil || conv.AdminID != c.Sub {
		writeErr(w, http.StatusForbidden, "Sin permiso")
		return
	}
	if conv.UserGuestNumber != nil {
		writeErr(w, http.StatusBadRequest, "No se puede dar plan a un invitado sin cuenta.")
		return
	}
	nombre := ""
	if conv.UserNombre != nil {
		nombre = *conv.UserNombre
	}
	res, err := s.dir.GrantPlan(r.Context(), conv.AppName, conv.UserEmail, nombre, plan)
	if err != nil {
		// Al panel se le responde genérico (no filtrar detalles internos), pero el
		// motivo REAL tiene que quedar en el log: los planes grupales tocan
		// Firestore (controlPagos + códigos) y sin esto el fallo era invisible.
		log.Printf("[chat] grant-plan FALLÓ app=%s email=%s plan=%q: %v",
			conv.AppName, conv.UserEmail, plan, err)
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	message := res.Message
	if message == "" {
		if res.OK {
			message = "Plan activado"
		} else {
			message = "No se pudo activar el plan"
		}
	}
	if res.OK {
		appName := appNombreCliente(conv.AppName)
		// 1) Push nativo (igual que hacía darPlan en JS): activar el plan sin aviso
		//    dejaba al usuario sin enterarse de que ya tenía acceso.
		_ = s.push.Push(r.Context(), conv.UserEmail, conv.AppName,
			"🎉 Tu plan ha sido activado",
			"¡Felicidades!. Ahora tienes acceso al Plan "+plan+".", "")
		// 2) Mensaje en el propio chat, redactado para el cliente.
		_ = s.postAutoText(r.Context(), b.ConversationID,
			"🎉 Por la compra de "+appName+" ya tienes acceso al Plan "+plan+" en "+appName+". ¡Gracias por tu compra!")
		// 3) Acceso cruzado (Yape↔BCP): por cada otra app, o ya se activó, o hay un
		//    código de 6 chars para activarla al descargarla. Se avisa por chat.
		for _, cg := range res.Cross {
			if cg.Codigo == "" {
				_ = s.postAutoText(r.Context(), b.ConversationID,
					"✅ También activamos tu acceso al Plan "+cg.Plan+" en "+appNombreCliente(cg.App)+".")
			} else {
				_ = s.postAutoText(r.Context(), b.ConversationID, mensajeCodigoCruzado(s.cfg.AppDownloadURL, cg))
			}
		}
	}
	status := http.StatusOK
	if !res.OK {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"ok": res.OK, "message": message, "plan": plan, "email": conv.UserEmail,
		"app": conv.AppName, "expiresAt": nullIfEmpty(res.FechaFinal), "cross": res.Cross,
	})
}

// mensajeCodigoCruzado arma el aviso con el código para activar la OTRA app,
// con el formato pedido: descarga + código de activación.
func mensajeCodigoCruzado(downloadURL string, cg CrossGrant) string {
	app := appNombreCliente(cg.App)
	return "📲 Descarga el app " + app + " aquí: " + downloadURL +
		"\nTu código de activación para el Plan " + cg.Plan + " es: " + cg.Codigo +
		"\nÁbrelo en la app " + app + " para activar tu acceso."
}

// appNombreCliente es el nombre comercial (de cara al cliente) de una app, sin el
// sufijo "Fake" interno: "Yape", "BCP", "Interbank".
func appNombreCliente(app string) string {
	switch normalizeApp(app) {
	case "bcp":
		return "BCP"
	case "interbank":
		return "Interbank"
	}
	return "Yape"
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

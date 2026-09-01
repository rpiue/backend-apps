package chat

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxMultipart = 64 << 20 // 64 MiB

// readUpload extrae el archivo "file" y los campos de un multipart form.
func readUpload(r *http.Request) (buf []byte, filename, mime string, fields map[string]string, err error) {
	if err = r.ParseMultipartForm(maxMultipart); err != nil {
		return nil, "", "", nil, err
	}
	fields = map[string]string{}
	for k, v := range r.MultipartForm.Value {
		if len(v) > 0 {
			fields[k] = v[0]
		}
	}
	file, header, ferr := r.FormFile("file")
	if ferr != nil {
		return nil, "", "", fields, nil // sin archivo (válido en algunos casos)
	}
	defer file.Close()
	buf, err = io.ReadAll(io.LimitReader(file, maxMultipart))
	if err != nil {
		return nil, "", "", fields, err
	}
	return buf, header.Filename, header.Header.Get("Content-Type"), fields, nil
}

func (s *Service) requireMessageAccess(w http.ResponseWriter, r *http.Request) *rawMessage {
	c := claimsOf(r)
	messageID, _ := strconv.ParseInt(chiParam(r, "messageId"), 10, 64)
	if messageID == 0 {
		writeErr(w, http.StatusBadRequest, "messageId requerido")
		return nil
	}
	msg, _ := s.db.getMessage(r.Context(), messageID)
	if msg == nil {
		writeErr(w, http.StatusNotFound, "Mensaje no encontrado")
		return nil
	}
	ok, _ := s.db.getConversationMember(r.Context(), msg.ConversationID, c.Sub)
	if !ok {
		writeErr(w, http.StatusForbidden, "Sin permiso")
		return nil
	}
	return msg
}

func (s *Service) postReaction(w http.ResponseWriter, r *http.Request) {
	msg := s.requireMessageAccess(w, r)
	if msg == nil {
		return
	}
	var b struct{ Emoji string }
	if !readBody(w, r, &b) {
		return
	}
	emoji := strings.TrimSpace(b.Emoji)
	if emoji == "" || len([]rune(emoji)) > 24 {
		writeErr(w, http.StatusBadRequest, "Emoji invalido")
		return
	}
	reactions, err := s.db.setMessageReaction(r.Context(), msg.ID, claimsOf(r).Sub, emoji)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	s.rt.broadcastConversation(msg.ConversationID, map[string]any{
		"type": "message_reaction", "conversationId": msg.ConversationID, "messageId": msg.ID, "reactions": reactions,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "messageId": msg.ID, "reactions": reactions})
}

func (s *Service) deleteReaction(w http.ResponseWriter, r *http.Request) {
	msg := s.requireMessageAccess(w, r)
	if msg == nil {
		return
	}
	var b struct{ Emoji string }
	_ = readBodyOptional(r, &b)
	emoji := strings.TrimSpace(b.Emoji)
	if emoji == "" {
		emoji = strings.TrimSpace(r.URL.Query().Get("emoji"))
	}
	if emoji == "" {
		writeErr(w, http.StatusBadRequest, "Emoji requerido")
		return
	}
	reactions, err := s.db.deleteMessageReaction(r.Context(), msg.ID, claimsOf(r).Sub, emoji)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	s.rt.broadcastConversation(msg.ConversationID, map[string]any{
		"type": "message_reaction", "conversationId": msg.ConversationID, "messageId": msg.ID, "reactions": reactions,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "messageId": msg.ID, "reactions": reactions})
}

func (s *Service) postPresence(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	var b struct {
		ConversationID    int64  `json:"conversationId"`
		LastSeenMessageID *int64 `json:"lastSeenMessageId"`
	}
	if !readBody(w, r, &b) {
		return
	}
	conversationID := convID(c, b.ConversationID)
	if conversationID == 0 {
		writeErr(w, http.StatusBadRequest, "conversationId requerido")
		return
	}
	ok, _ := s.db.getConversationMember(r.Context(), conversationID, c.Sub)
	if !ok {
		writeErr(w, http.StatusForbidden, "Sin permiso")
		return
	}
	_, _ = s.db.markConversationPresence(r.Context(), conversationID, c.Role, b.LastSeenMessageID)
	full := s.rt.presencePayload(r.Context(), conversationID)
	// Difunde la presencia a la conversación para que el "visto" (last_seen) y el
	// estado en línea lleguen en vivo a la otra parte, no solo a quien la marcó.
	s.rt.broadcastConversation(conversationID, map[string]any{"type": "presence", "conversationId": conversationID, "presence": full})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "presence": full})
}

func (s *Service) getOnline(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	conversationID, _ := strconv.ParseInt(chiParam(r, "conversationId"), 10, 64)
	ok, _ := s.db.getConversationMember(r.Context(), conversationID, c.Sub)
	if !ok {
		writeErr(w, http.StatusForbidden, "Sin permiso")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "online": s.rt.getConversationOnline(conversationID)})
}

func (s *Service) postAttachments(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	buf, filename, mime, fields, err := readUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Archivo no permitido")
		return
	}
	if len(buf) == 0 {
		writeErr(w, http.StatusBadRequest, "Archivo no permitido")
		return
	}
	var adminVal int64
	if v := fields["conversationId"]; v != "" {
		adminVal, _ = strconv.ParseInt(v, 10, 64)
	}
	conversationID := convID(c, adminVal)
	if conversationID == 0 {
		writeErr(w, http.StatusBadRequest, "conversationId requerido")
		return
	}
	ok, _ := s.db.getConversationMember(r.Context(), conversationID, c.Sub)
	if !ok {
		writeErr(w, http.StatusForbidden, "Sin permiso")
		return
	}
	enc := encFromFields(fields)
	res, err := s.createAttachmentMessage(r.Context(), conversationID, c.Sub, c.Role, buf, mime, filename, enc,
		fields["clientNonce"], replyFromFields(fields), uploadText(fields), durationFromFields(fields))
	if err != nil {
		writeChatErr(w, err)
		return
	}
	_, _ = s.db.markConversationPresence(r.Context(), conversationID, c.Role, &res.message.ID)
	s.notifyUserIfOffline(conversationID, c.Role)
	s.rt.broadcastConversation(conversationID, map[string]any{"type": "message", "conversationId": conversationID, "message": res.message})
	s.rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": conversationID})
	if c.Role == "user" {
		s.notifyAdminIfOffline(conversationID, "Un usuario envio un archivo.")
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "message": res.message})
}

func (s *Service) getAttachmentFile(w http.ResponseWriter, r *http.Request) {
	c := claimsOf(r)
	id, _ := strconv.ParseInt(chiParam(r, "id"), 10, 64)
	att, _ := s.db.getAttachment(r.Context(), id)
	if att == nil {
		writeErr(w, http.StatusNotFound, "No encontrado")
		return
	}
	ok, _ := s.db.getConversationMember(r.Context(), att.ConversationID, c.Sub)
	if !ok {
		writeErr(w, http.StatusForbidden, "Sin permiso")
		return
	}
	isImage := strings.HasPrefix(att.MimeType, "image/")
	download := r.URL.Query().Get("download") == "1"
	if download && !isImage {
		writeErr(w, http.StatusForbidden, "Solo imagenes pueden descargarse")
		return
	}
	storagePath, _ := filepath.Abs(att.StoragePath)
	f, err := os.Open(storagePath)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Archivo no encontrado")
		return
	}
	defer f.Close()

	// El Content-Type NO se toma tal cual de la base de datos: las filas
	// antiguas las escribió el sistema anterior, que se fiaba de lo que
	// declaraba el cliente. Si una de ellas dijera "text/html", lo estaríamos
	// sirviendo como HTML. tipoSeguro lo reduce a la lista de tipos de media
	// conocidos y, ante cualquier otra cosa, entrega un flujo de bytes opaco.
	w.Header().Set("Content-Type", tipoSeguro(att.MimeType))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// CSP restrictiva + sandbox: si un navegador (p.ej. el panel admin) abre la
	// URL del adjunto directamente, el contenido queda inerte (no ejecuta scripts
	// ni carga recursos), neutralizando polyglots/HTML disfrazado de media.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "private, no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	disp := "inline"
	if download {
		disp = "attachment"
	}
	name := "archivo"
	if att.OriginalName != nil {
		name = *att.OriginalName
	}
	w.Header().Set("Content-Disposition", disp+`; filename="`+safeDownloadName(name)+`"`)

	if att.StorageEncoding == "gzip" {
		gz, err := gzipReader(f)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Error al leer archivo")
			return
		}
		defer gz.Close()
		// Cap defensivo: aunque el blob gzip lo generó el servidor, se limita la
		// expansión al máximo permitido por tipo para no arriesgar OOM ante un
		// blob corrupto/manipulado en disco.
		_, _ = io.Copy(w, io.LimitReader(gz, maxFileBytes+1024))
		return
	}
	_, _ = io.Copy(w, io.LimitReader(f, maxFileBytes+1024))
}

// --- Endpoints "support" (autentican por email/pin en el body) ---

func (s *Service) postSupportMessage(w http.ResponseWriter, r *http.Request) {
	var b messageBody
	if !readBody(w, r, &b) {
		return
	}
	pin := b.Pin
	if pin == "" {
		pin = b.Clave
	}
	if !s.permitirIntentoAuth(w, r, "support", b.Email) {
		return
	}
	auth, err := s.authenticateUserSession(r.Context(), b.App, b.Email, pin)
	if err != nil {
		s.registrarFalloAuth(r, "support", b.Email)
		writeChatErrOr(w, err, http.StatusUnauthorized, "Email o pin incorrecto")
		return
	}
	s.limpiarIntentosAuth(r, "support", b.Email)
	if !s.rateOK(w, r, auth.conversation.ID) {
		return
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
	fp, sig, err := s.enforceUserSendGuard(r.Context(), "user", auth.conversation.ID, auth.user.ID, b.textValue(), nil, nil)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	created, err := s.db.createMessage(r.Context(), auth.conversation.ID, auth.user.ID, "text", *enc, b.ClientNonce, fp, sig, b.replyToID())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error interno del chat")
		return
	}
	before := created.ID - 1
	msgs, _ := s.db.listMessages(r.Context(), auth.conversation.ID, &before, nil, 1)
	if len(msgs) > 0 {
		s.rt.broadcastConversation(auth.conversation.ID, map[string]any{"type": "message", "conversationId": auth.conversation.ID, "message": msgs[0]})
	}
	s.rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": auth.conversation.ID, "app": auth.conversation.App})
	s.notifyAdminIfOffline(auth.conversation.ID, "Un usuario envio un mensaje.")
	s.notifyPlanLeadIfAdminOffline(auth.conversation.ID, auth.conversation.App, b.textValue())
	// Respuesta rápida primero; si ninguna coincide, pasa a la IA (batching).
	if !s.maybeAutoReply(r.Context(), auth.conversation.ID, auth.conversation.App, b.textValue()) {
		s.aiEnqueue(auth.conversation.ID, b.textValue())
	}
	var out any
	if len(msgs) > 0 {
		out = msgs[0]
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "user": auth.user, "conversation": auth.conversation, "theme": themeForApp(auth.nameApp), "message": out})
}

func (s *Service) postSupportAttachments(w http.ResponseWriter, r *http.Request) {
	buf, filename, mime, fields, err := readUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Archivo no permitido")
		return
	}
	pin := fields["pin"]
	if pin == "" {
		pin = fields["clave"]
	}
	if !s.permitirIntentoAuth(w, r, "support", fields["email"]) {
		return
	}
	auth, err := s.authenticateUserSession(r.Context(), fields["app"], fields["email"], pin)
	if err != nil {
		s.registrarFalloAuth(r, "support", fields["email"])
		writeChatErrOr(w, err, http.StatusUnauthorized, "Email o pin incorrecto")
		return
	}
	s.limpiarIntentosAuth(r, "support", fields["email"])
	if !s.rateOK(w, r, auth.conversation.ID) {
		return
	}
	enc := encFromFields(fields)
	res, err := s.createAttachmentMessage(r.Context(), auth.conversation.ID, auth.user.ID, "user", buf, mime, filename, enc,
		fields["clientNonce"], replyFromFields(fields), uploadText(fields), durationFromFields(fields))
	if err != nil {
		writeChatErrOr(w, err, http.StatusUnauthorized, "Email o pin incorrecto")
		return
	}
	_, _ = s.db.markConversationPresence(r.Context(), auth.conversation.ID, "user", &res.message.ID)
	s.rt.broadcastConversation(auth.conversation.ID, map[string]any{"type": "message", "conversationId": auth.conversation.ID, "message": res.message})
	s.rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": auth.conversation.ID, "app": auth.conversation.App})
	s.notifyAdminIfOffline(auth.conversation.ID, "Un usuario envio un archivo.")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "user": auth.user, "conversation": auth.conversation, "message": res.message})
}

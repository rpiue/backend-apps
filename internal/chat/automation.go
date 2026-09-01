package chat

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// findAutomationReply replica findAutomationReply (mejor patrón por textScore).
func (s *Service) findAutomationReply(ctx context.Context, app, text string) *Automation {
	automations, err := s.db.listAutomations(ctx, app, true)
	if err != nil {
		return nil
	}
	var best *Automation
	var bestScore float64
	for i := range automations {
		a := &automations[i]
		for _, pattern := range a.Patterns {
			score := textScore(text, pattern)
			if best == nil || score > bestScore {
				best = a
				bestScore = score
			}
		}
	}
	if best == nil {
		return nil
	}
	minScore := best.MinScore
	if minScore == 0 {
		minScore = 0.45
	}
	if bestScore < minScore {
		return nil
	}
	return best
}

// maybeAutoReply responde automáticamente (+ QR/media si aplica). Devuelve true si
// una respuesta rápida COINCIDIÓ con el mensaje (aunque se haya deduplicado), para
// que el llamador NO pase el mensaje a la IA.
func (s *Service) maybeAutoReply(ctx context.Context, conversationID int64, app, userText string) bool {
	automation := s.findAutomationReply(ctx, app, userText)
	if automation == nil {
		return false // no hubo match → que responda la IA
	}
	questionHash := sha256Hex("automation:" + strconv.FormatInt(automation.ID, 10))
	can, _ := s.db.shouldSendAutomationReply(ctx, conversationID, automation.ID, questionHash, 5)
	if !can {
		return true // coincidió pero ya se respondió hace poco (dedupe) → no IA
	}
	details, _ := s.db.getConversationDetails(ctx, conversationID)
	if details == nil {
		return true
	}
	senderID := details.AdminID

	if automation.SendPaymentQR {
		if qrMsg := s.createAutomationPaymentQrMessage(ctx, conversationID, senderID, automation.ID); qrMsg != nil {
			s.rt.broadcastConversation(conversationID, map[string]any{
				"type": "message", "conversationId": conversationID, "message": qrMsg,
			})
		}
	}

	// Media configurado (imagen/video): se envía como mensaje antes del texto.
	if automation.Attachment != nil {
		if m := s.createAutomationAttachmentMessage(ctx, conversationID, senderID, automation.Attachment); m != nil {
			s.rt.broadcastConversation(conversationID, map[string]any{
				"type": "message", "conversationId": conversationID, "message": m,
			})
		}
	}

	// Respuesta solo-media (sin texto): no mandes una burbuja de texto vacía.
	if strings.TrimSpace(automation.Response) == "" {
		s.notifyUserIfOffline(conversationID, "admin")
		return true
	}

	enc := Encrypted{
		Ciphertext: base64.StdEncoding.EncodeToString([]byte(automation.Response)),
		IV:         "automation",
		Tag:        "",
	}
	created, err := s.db.createMessage(ctx, conversationID, senderID, "text", enc,
		"auto-"+strconv.FormatInt(automation.ID, 10)+"-"+strconv.FormatInt(time.Now().UnixMilli(), 10), nil, nil, nil)
	if err != nil {
		return true
	}
	before := created.ID - 1
	msgs, _ := s.db.listMessages(ctx, conversationID, &before, nil, 1)
	if len(msgs) == 0 {
		return true
	}
	// IMPORTANTE: la auto-respuesta la manda el BOT, no un humano. NO se debe
	// avanzar last_admin_seen_message_id (antes hacía markConversationPresence con
	// created.ID), porque eso marcaba como "visto" el mensaje del cliente y la
	// conversación dejaba de contar como no leída aunque nadie del soporte la viera.
	s.rt.broadcastConversation(conversationID, map[string]any{
		"type": "message", "conversationId": conversationID, "message": msgs[0],
	})
	s.notifyUserIfOffline(conversationID, "admin")
	return true
}

// postAutoText inserta y difunde un mensaje de texto automático en la conversación
// como si lo enviara el soporte (usa el admin de la conversación como remitente,
// con IV "automation" que el cliente decodifica como texto plano). Igual que las
// auto-respuestas, NO avanza el "visto" del admin. Devuelve false si falla.
func (s *Service) postAutoText(ctx context.Context, conversationID int64, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	details, _ := s.db.getConversationDetails(ctx, conversationID)
	if details == nil {
		return false
	}
	enc := Encrypted{
		Ciphertext: base64.StdEncoding.EncodeToString([]byte(text)),
		IV:         "automation",
		Tag:        "",
	}
	nonce := "grant-" + strconv.FormatInt(conversationID, 10) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	created, err := s.db.createMessage(ctx, conversationID, details.AdminID, "text", enc, nonce, nil, nil, nil)
	if err != nil {
		return false
	}
	before := created.ID - 1
	msgs, _ := s.db.listMessages(ctx, conversationID, &before, nil, 1)
	if len(msgs) == 0 {
		return false
	}
	s.rt.broadcastConversation(conversationID, map[string]any{
		"type": "message", "conversationId": conversationID, "message": msgs[0],
	})
	s.notifyUserIfOffline(conversationID, "admin")
	return true
}

// createAutomationPaymentQrMessage adjunta el QR de pago como mensaje imagen.
func (s *Service) createAutomationPaymentQrMessage(ctx context.Context, conversationID, senderID, automationID int64) *Message {
	storagePath, _ := filepath.Abs(s.cfg.PaymentQRPath)
	buf, err := os.ReadFile(storagePath)
	if err != nil {
		return nil
	}
	st, err := os.Stat(storagePath)
	if err != nil {
		return nil
	}
	mime := detectMimeFromBuffer(buf)
	if mime == "" {
		mime = "image/jpeg"
	}
	nonce := "auto-qr-" + strconv.FormatInt(automationID, 10) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "-" + newUUID()
	created, err := s.db.createMessage(ctx, conversationID, senderID, "image",
		Encrypted{Ciphertext: "", IV: "automation", Tag: ""}, nonce, nil, nil, nil)
	if err != nil {
		return nil
	}
	if err := s.db.createAttachment(ctx, created.ID, senderID, fileMeta{
		Path: storagePath, OriginalName: filepath.Base(storagePath), MimeType: mime,
		Size: st.Size(), SHA256: sha256OfBytes(buf), StorageEncoding: "identity",
	}); err != nil {
		return nil
	}
	before := created.ID - 1
	msgs, _ := s.db.listMessages(ctx, conversationID, &before, nil, 1)
	if len(msgs) == 0 {
		return nil
	}
	return &msgs[0]
}

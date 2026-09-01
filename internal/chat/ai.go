package chat

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"codex/backend/internal/ai"
)

// escalarSentinel: si el modelo lo devuelve, NO se responde al usuario (se deja
// el mensaje como no leído para que lo atienda un humano).
const escalarSentinel = "[ESCALAR]"

// defaultAIPrompt es el prompt de sistema por defecto (con guardrails). El admin
// puede sobreescribirlo desde el panel (se guarda en Redis ai:prompt).
const defaultAIPrompt = `Eres el asistente de soporte de la app de pagos "Codex".
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
   los "Datos del usuario actual" que se te den. Si preguntan por OTRA persona (otro
   correo o usuario), responde amablemente: "Por seguridad no puedo dar datos de otras
   cuentas. ¿Para qué lo necesitas?" y NADA más.
4. Si piden LISTADOS o datos agregados (p. ej. "qué usuarios tienen plan", "cuántos
   hay"), NO los des; responde: "Eso no lo puedo consultar aquí. ¿Quieres que le pida
   un reporte a soporte?".
5. No inventes ni completes datos que no tengas. Si NO entiendes la pregunta, no sabes
   la respuesta con certeza, o el tema no es soporte, responde EXACTAMENTE (sin texto
   adicional): ` + escalarSentinel

// SetAI conecta el cliente de IA y arranca el batching (debounce de 5s). El typing
// del usuario reinicia el timer (espera a que termine de escribir).
func (s *Service) SetAI(client *ai.Client) {
	s.ai = client
	s.aiDeb = ai.NewDebouncer(5 * time.Second)
	s.aiBuf = map[int64][]string{}
	s.rt.setUserTypingHandler(func(conversationID int64) { s.aiTouch(conversationID) })
}

func (s *Service) aiEnabled(ctx context.Context) bool {
	if s.ai == nil || s.cache == nil {
		return false
	}
	v, _ := s.cache.Client().Get(ctx, "ai:enabled").Result()
	return v == "1"
}

// aiEnqueue agrega el mensaje del usuario al buffer y (re)inicia el timer de 5s.
// Lo llama el flujo de mensajes cuando NO hubo respuesta rápida (automation).
func (s *Service) aiEnqueue(conversationID int64, text string) {
	if s.ai == nil || !s.aiEnabled(context.Background()) {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.aiBufMu.Lock()
	s.aiBuf[conversationID] = append(s.aiBuf[conversationID], text)
	s.aiBufMu.Unlock()
	s.aiTouch(conversationID)
}

// aiTouch reinicia el timer de 5s (por un mensaje nuevo o porque el usuario está
// escribiendo). Solo hace algo si hay mensajes en el buffer.
func (s *Service) aiTouch(conversationID int64) {
	if s.ai == nil || s.aiDeb == nil {
		return
	}
	s.aiBufMu.Lock()
	pending := len(s.aiBuf[conversationID]) > 0
	s.aiBufMu.Unlock()
	if !pending {
		return
	}
	s.aiDeb.Trigger(strconv.FormatInt(conversationID, 10), func() { s.aiFlush(conversationID) })
}

// aiFlush se dispara tras 5s sin actividad: junta los mensajes, consulta la IA y
// responde. Si la IA escala/no sabe, NO responde (deja el mensaje como no leído).
func (s *Service) aiFlush(conversationID int64) {
	s.aiBufMu.Lock()
	batch := s.aiBuf[conversationID]
	delete(s.aiBuf, conversationID)
	s.aiBufMu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
	defer cancel()
	if !s.aiEnabled(ctx) {
		return
	}
	details, _ := s.db.getConversationDetails(ctx, conversationID)
	if details == nil {
		return
	}
	// Si un admin está atendiendo la conversación en vivo, la IA NO interrumpe.
	if s.rt.getConversationOnline(conversationID).AdminOnline {
		return
	}

	msgs := []ai.Msg{
		{Role: "system", Content: s.aiSystemPrompt(ctx, details)},
		{Role: "user", Content: strings.Join(batch, "\n")},
	}
	resp, err := s.ai.Chat(ctx, s.aiModel(ctx), msgs)
	resp = strings.TrimSpace(resp)

	// Escalar: error, vacío o sentinela → NO responder. El mensaje del usuario
	// queda SIN LEER para que lo atienda un humano; se avisa al admin.
	if err != nil || resp == "" || strings.Contains(strings.ToUpper(resp), escalarSentinel) {
		s.notifyAdminIfOffline(conversationID, "Un usuario necesita atención (la IA no pudo resolverlo).")
		return
	}

	// La IA respondió: se postea como mensaje del admin (bot) SIN avanzar
	// last_admin_seen_message_id, así el mensaje del usuario sigue contando como
	// NO leído en el panel (lo respondió la máquina, no un humano).
	senderID := details.AdminID
	enc := Encrypted{Ciphertext: base64.StdEncoding.EncodeToString([]byte(resp)), IV: "automation", Tag: ""}
	created, err := s.db.createMessage(ctx, conversationID, senderID, "text", enc,
		"ai-"+strconv.FormatInt(time.Now().UnixMilli(), 10), nil, nil, nil)
	if err != nil {
		return
	}
	before := created.ID - 1
	out, _ := s.db.listMessages(ctx, conversationID, &before, nil, 1)
	if len(out) == 0 {
		return
	}
	s.rt.broadcastConversation(conversationID, map[string]any{"type": "message", "conversationId": conversationID, "message": out[0]})
	s.rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": conversationID})
	s.notifyUserIfOffline(conversationID, "admin")
}

// aiSystemPrompt = prompt configurado (o el default con guardrails) + los datos
// SOLO del usuario de esta conversación (para que responda su cuenta sin filtrar
// datos de otros: físicamente nunca ve los de nadie más).
func (s *Service) aiSystemPrompt(ctx context.Context, d *ConversationDetails) string {
	prompt := defaultAIPrompt
	if s.cache != nil {
		if v, _ := s.cache.Client().Get(ctx, "ai:prompt").Result(); strings.TrimSpace(v) != "" {
			prompt = v
		}
	}
	if datos := s.aiUserData(ctx, d); datos != "" {
		prompt += "\n\nDatos del usuario actual (SOLO para responder SU cuenta; NO son de otras personas):\n" + datos
	}
	return prompt
}

func (s *Service) aiModel(ctx context.Context) string {
	if s.cache != nil {
		if v, _ := s.cache.Client().Get(ctx, "ai:model").Result(); strings.TrimSpace(v) != "" {
			return v
		}
	}
	return s.ai.DefaultModel()
}

// aiUserData obtiene los datos del PROPIO usuario de la conversación (email, plan,
// vencimiento). Nunca consulta otros usuarios.
func (s *Service) aiUserData(ctx context.Context, d *ConversationDetails) string {
	if d.UserEmail == "" || s.dir == nil {
		return ""
	}
	users, _, err := s.dir.Lookup(ctx, d.AppName, d.UserEmail, nil)
	if err != nil || len(users) == 0 {
		return ""
	}
	u := users[0]
	var b strings.Builder
	b.WriteString("- Email: " + d.UserEmail + "\n")
	if u.Nombre != nil && *u.Nombre != "" {
		b.WriteString("- Nombre: " + *u.Nombre + "\n")
	}
	b.WriteString("- App: " + d.AppName + "\n")
	if u.Plan != nil && *u.Plan != "" {
		b.WriteString("- Plan: " + *u.Plan + "\n")
	}
	if u.FechaFinal != nil && *u.FechaFinal != "" {
		b.WriteString("- Vence: " + *u.FechaFinal + "\n")
	}
	if u.Acceso != nil && *u.Acceso != "" {
		b.WriteString("- Acceso: " + *u.Acceso + "\n")
	}
	return b.String()
}

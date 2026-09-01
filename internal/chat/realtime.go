package chat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"codex/backend/internal/middleware"

	secmw "codex/backend/internal/middleware"
)

// typingTTL es lo que dura un "escribiendo" sin refresco antes de que el
// servidor lo apague solo. Protege al receptor de quedarse con el indicador
// colgado si el emisor se cae sin mandar isTyping:false.
const typingTTL = 6 * time.Second

// wsClient es una conexión WebSocket autenticada (port de las props ws.* del JS).
type wsClient struct {
	conn          *websocket.Conn
	user          *SessionRow
	subscriptions map[int64]struct{}
	lastSeenMsgID *int64
	sendMu        sync.Mutex
	alive         bool

	// typingIn: conversación -> instante del último typing:true. Lo protege
	// Realtime.mu (igual que subscriptions).
	typingIn map[int64]time.Time
}

func (c *wsClient) send(payload any) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.conn.WriteJSON(payload)
}

// Realtime porta chat/realtime.js: hub en memoria de presencia + broadcast.
type Realtime struct {
	db       *DB
	upgrader websocket.Upgrader
	// lim aporta la resolución de IP consciente de TRUSTED_PROXIES. El ticket
	// del WebSocket va atado al hash de la IP, así que si esa IP se pudiera
	// falsificar con una cabecera, la atadura no serviría para nada.
	lim *middleware.Limiter

	mu                    sync.RWMutex
	clientsByConversation map[int64]map[*wsClient]struct{}
	adminClients          map[*wsClient]struct{}
	typingClients         map[*wsClient]struct{} // los que tienen algún typing activo

	onUserConnected func(conversationID int64)
	onUserTyping    func(conversationID int64)
}

func newRealtime(db *DB, allowedOrigins []string, lim *middleware.Limiter) *Realtime {
	rt := &Realtime{
		db:                    db,
		lim:                   lim,
		upgrader:              websocket.Upgrader{CheckOrigin: secmw.OriginChecker(allowedOrigins), ReadBufferSize: 4096, WriteBufferSize: 4096},
		clientsByConversation: map[int64]map[*wsClient]struct{}{},
		adminClients:          map[*wsClient]struct{}{},
		typingClients:         map[*wsClient]struct{}{},
	}
	go rt.typingJanitor()
	return rt
}

func (rt *Realtime) setUserConnectedHandler(h func(int64)) { rt.onUserConnected = h }
func (rt *Realtime) setUserTypingHandler(h func(int64))    { rt.onUserTyping = h }

func (rt *Realtime) conversationSet(id int64) map[*wsClient]struct{} {
	if rt.clientsByConversation[id] == nil {
		rt.clientsByConversation[id] = map[*wsClient]struct{}{}
	}
	return rt.clientsByConversation[id]
}

func (rt *Realtime) removeFromConversation(ws *wsClient, id int64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	set := rt.clientsByConversation[id]
	if set == nil {
		return
	}
	delete(set, ws)
	if len(set) == 0 {
		delete(rt.clientsByConversation, id)
	}
}

func (rt *Realtime) broadcastConversation(id int64, payload any) {
	rt.mu.RLock()
	set := rt.clientsByConversation[id]
	clients := make([]*wsClient, 0, len(set))
	for ws := range set {
		clients = append(clients, ws)
	}
	rt.mu.RUnlock()
	for _, ws := range clients {
		ws.send(payload)
	}
}

func (rt *Realtime) broadcastConversationExcept(id int64, except *wsClient, payload any) {
	rt.mu.RLock()
	set := rt.clientsByConversation[id]
	clients := make([]*wsClient, 0, len(set))
	for ws := range set {
		if ws != except {
			clients = append(clients, ws)
		}
	}
	rt.mu.RUnlock()
	for _, ws := range clients {
		ws.send(payload)
	}
}

// fanoutTyping reparte un evento "typing" a los suscritos de la conversación
// (menos el emisor) Y a TODOS los paneles admin, deduplicando. Los paneles
// necesitan el typing de conversaciones que no tienen abiertas para poder
// contar cuántos clientes están escribiendo a la vez.
func (rt *Realtime) fanoutTyping(conversationID int64, sender *wsClient, payload any) {
	rt.mu.RLock()
	seen := make(map[*wsClient]struct{}, len(rt.adminClients)+2)
	clients := make([]*wsClient, 0, len(rt.adminClients)+2)
	for ws := range rt.clientsByConversation[conversationID] {
		if ws == sender {
			continue
		}
		seen[ws] = struct{}{}
		clients = append(clients, ws)
	}
	for ws := range rt.adminClients {
		if ws == sender {
			continue
		}
		if _, dup := seen[ws]; dup {
			continue
		}
		clients = append(clients, ws)
	}
	rt.mu.RUnlock()
	for _, ws := range clients {
		ws.send(payload)
	}
}

// emitTyping publica el estado de escritura de un emisor. El typing de un
// cliente llega a todos los paneles; el del admin solo a su conversación.
func (rt *Realtime) emitTyping(ws *wsClient, conversationID int64, isTyping bool) {
	payload := map[string]any{
		"type": "typing", "conversationId": conversationID, "senderId": ws.user.UserID,
		"senderRole": ws.user.Role, "isTyping": isTyping, "at": time.Now().UnixMilli(),
	}
	if ws.user.Role == "user" {
		// El usuario está escribiendo → reinicia el timer de batching de la IA.
		if isTyping && rt.onUserTyping != nil {
			rt.onUserTyping(conversationID)
		}
		rt.fanoutTyping(conversationID, ws, payload)
		return
	}
	rt.broadcastConversationExcept(conversationID, ws, payload)
}

// setTypingState registra/limpia el typing de una conexión (para expirarlo).
func (rt *Realtime) setTypingState(ws *wsClient, conversationID int64, isTyping bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if isTyping {
		if ws.typingIn == nil {
			ws.typingIn = map[int64]time.Time{}
		}
		ws.typingIn[conversationID] = time.Now()
		rt.typingClients[ws] = struct{}{}
		return
	}
	delete(ws.typingIn, conversationID)
	if len(ws.typingIn) == 0 {
		delete(rt.typingClients, ws)
	}
}

// clearTyping saca a una conexión del registro de typing y devuelve las
// conversaciones donde estaba escribiendo (para avisar que ya no lo hace).
func (rt *Realtime) clearTyping(ws *wsClient) []int64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(ws.typingIn) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(ws.typingIn))
	for id := range ws.typingIn {
		ids = append(ids, id)
	}
	ws.typingIn = nil
	delete(rt.typingClients, ws)
	return ids
}

// typingJanitor apaga los "escribiendo" que llevan más de typingTTL sin
// refrescarse (emisor caído o evento isTyping:false perdido).
func (rt *Realtime) typingJanitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	type stale struct {
		ws             *wsClient
		conversationID int64
	}
	for now := range ticker.C {
		var expired []stale
		rt.mu.Lock()
		for ws := range rt.typingClients {
			for id, at := range ws.typingIn {
				if now.Sub(at) > typingTTL {
					delete(ws.typingIn, id)
					expired = append(expired, stale{ws, id})
				}
			}
			if len(ws.typingIn) == 0 {
				delete(rt.typingClients, ws)
			}
		}
		rt.mu.Unlock()
		for _, e := range expired {
			rt.emitTyping(e.ws, e.conversationID, false)
		}
	}
}

func (rt *Realtime) broadcastAdmin(payload any) {
	rt.mu.RLock()
	clients := make([]*wsClient, 0, len(rt.adminClients))
	for ws := range rt.adminClients {
		clients = append(clients, ws)
	}
	rt.mu.RUnlock()
	for _, ws := range clients {
		ws.send(payload)
	}
}

func (rt *Realtime) deleteConversationFromRealtime(conversationID int64) {
	rt.mu.Lock()
	set := rt.clientsByConversation[conversationID]
	notified := map[*wsClient]struct{}{}
	var convClients, adminClients []*wsClient
	if set != nil {
		for ws := range set {
			convClients = append(convClients, ws)
			notified[ws] = struct{}{}
			delete(ws.subscriptions, conversationID)
		}
		delete(rt.clientsByConversation, conversationID)
	}
	for ws := range rt.adminClients {
		if _, ok := notified[ws]; !ok {
			adminClients = append(adminClients, ws)
		}
	}
	rt.mu.Unlock()
	payload := map[string]any{"type": "conversation_deleted", "conversationId": conversationID}
	for _, ws := range convClients {
		ws.send(payload)
	}
	for _, ws := range adminClients {
		ws.send(payload)
	}
}

func (rt *Realtime) isConversationUserOnline(conversationID int64) bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for ws := range rt.clientsByConversation[conversationID] {
		if ws.user != nil && ws.user.Role == "user" {
			return true
		}
	}
	return false
}

func (rt *Realtime) isAnyAdminOnline() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.adminClients) > 0
}

func (rt *Realtime) getConversationOnline(conversationID int64) Online {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	var o Online
	for ws := range rt.clientsByConversation[conversationID] {
		if ws.user == nil {
			continue
		}
		if ws.user.Role == "user" {
			o.UserOnline = true
		}
		if ws.user.Role == "admin" {
			o.AdminOnline = true
		}
	}
	return o
}

func (rt *Realtime) presencePayload(ctx context.Context, conversationID int64) map[string]any {
	p, _ := rt.db.getConversationPresence(ctx, conversationID)
	online := rt.getConversationOnline(conversationID)
	out := map[string]any{"online": online}
	if p != nil {
		out["id"] = p.ID
		out["last_user_presence_at"] = p.LastUserPresenceAt
		out["last_admin_presence_at"] = p.LastAdminPresenceAt
		out["last_user_seen_message_id"] = p.LastUserSeenMessageID
		out["last_admin_seen_message_id"] = p.LastAdminSeenMessageID
		out["user_online"] = p.UserOnline
		out["admin_online"] = p.AdminOnline
	}
	return out
}

func (rt *Realtime) canAccess(ctx context.Context, user *SessionRow, conversationID int64) bool {
	if conversationID == 0 {
		return false
	}
	if user.Role == "admin" {
		c, _ := rt.db.getConversation(ctx, conversationID)
		return c != nil && c.AdminID == user.UserID
	}
	ok, _ := rt.db.getConversationMember(ctx, conversationID, user.UserID)
	return ok
}

func (rt *Realtime) subscribe(ctx context.Context, ws *wsClient, conversationID int64) bool {
	if conversationID == 0 || !rt.canAccess(ctx, ws.user, conversationID) {
		return false
	}
	rt.mu.Lock()
	rt.conversationSet(conversationID)[ws] = struct{}{}
	ws.subscriptions[conversationID] = struct{}{}
	rt.mu.Unlock()

	_, _ = rt.db.markConversationPresence(ctx, conversationID, ws.user.Role, ws.lastSeenMsgID)
	presence := rt.presencePayload(ctx, conversationID)
	if ws.user.Role == "user" {
		if rt.onUserConnected != nil {
			rt.onUserConnected(conversationID)
		}
	} else if ws.user.Role == "admin" {
		_ = rt.db.clearNotificationDedupeKey(ctx, "admin:"+strconv.FormatInt(conversationID, 10))
	}
	ws.send(map[string]any{"type": "subscribed", "conversationId": conversationID, "presence": presence})
	rt.broadcastConversation(conversationID, map[string]any{"type": "presence", "conversationId": conversationID, "presence": presence})
	// Avisa a los paneles admin para que el estado "en línea" de la lista se
	// actualice en vivo cuando el usuario (o un admin) se conecta a la conversación.
	rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": conversationID, "presence": presence})
	return true
}

func (rt *Realtime) detach(ctx context.Context, ws *wsClient) {
	// Si se cae mientras escribía, apaga su "escribiendo" en los receptores.
	for _, id := range rt.clearTyping(ws) {
		rt.emitTyping(ws, id, false)
	}
	rt.mu.Lock()
	affected := make([]int64, 0, len(ws.subscriptions))
	for id := range ws.subscriptions {
		affected = append(affected, id)
		if set := rt.clientsByConversation[id]; set != nil {
			delete(set, ws)
			if len(set) == 0 {
				delete(rt.clientsByConversation, id)
			}
		}
	}
	delete(rt.adminClients, ws)
	rt.mu.Unlock()
	for _, id := range affected {
		presence := rt.presencePayload(ctx, id)
		rt.broadcastConversation(id, map[string]any{"type": "presence", "conversationId": id, "presence": presence})
		rt.broadcastAdmin(map[string]any{"type": "conversation_update", "conversationId": id, "presence": presence})
	}
}

// HandleWS atiende GET /api/chat/ws?ticket=... (upgrade + loop de mensajes).
func (rt *Realtime) HandleWS(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	fp := rt.fingerprintFromRequest(r)
	decoded, err := rt.db.consumeWsTicket(r.Context(), ticket, fp.userAgentHash, fp.ipHash)
	if err != nil || decoded == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	conn, err := rt.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws := &wsClient{conn: conn, user: decoded, subscriptions: map[int64]struct{}{}, alive: true}
	conn.SetReadLimit(32 * 1024)
	conn.SetPongHandler(func(string) error { ws.alive = true; return nil })

	ctx := context.Background()
	rt.mu.Lock()
	if decoded.Role == "admin" {
		rt.adminClients[ws] = struct{}{}
	}
	rt.mu.Unlock()
	if decoded.Role == "admin" {
		_ = rt.db.clearNotificationDedupePrefix(ctx, "admin:")
	}
	if decoded.Role == "user" && decoded.ConversationID != nil {
		rt.subscribe(ctx, ws, *decoded.ConversationID)
	}
	ws.send(map[string]any{"type": "hello", "role": decoded.Role})

	go rt.pingLoop(ws)

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		ws.alive = true
		rt.handleClientMessage(ctx, ws, raw)
	}
	rt.detach(ctx, ws)
	_ = conn.Close()
}

func (rt *Realtime) pingLoop(ws *wsClient) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !ws.alive {
			_ = ws.conn.Close()
			return
		}
		ws.alive = false
		ws.sendMu.Lock()
		_ = ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := ws.conn.WriteMessage(websocket.PingMessage, nil)
		ws.sendMu.Unlock()
		if err != nil {
			return
		}
	}
}

func (rt *Realtime) handleClientMessage(ctx context.Context, ws *wsClient, raw []byte) {
	var data struct {
		Type              string `json:"type"`
		ConversationID    any    `json:"conversationId"`
		LastSeenMessageID *int64 `json:"lastSeenMessageId"`
		IsTyping          *bool  `json:"isTyping"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		ws.send(map[string]any{"type": "error", "error": "JSON invalido"})
		return
	}
	switch data.Type {
	case "ping":
		ws.send(map[string]any{"type": "pong", "at": time.Now().UnixMilli()})
	case "subscribe":
		if ws.user.Role != "admin" {
			return
		}
		id := toInt64(data.ConversationID)
		if !rt.subscribe(ctx, ws, id) {
			ws.send(map[string]any{"type": "error", "error": "Sin permiso"})
		}
	case "unsubscribe":
		id := toInt64(data.ConversationID)
		rt.removeFromConversation(ws, id)
		delete(ws.subscriptions, id)
		ws.send(map[string]any{"type": "unsubscribed", "conversationId": id})
	case "presence":
		id := rt.targetConversation(ws, data.ConversationID)
		if !rt.canAccess(ctx, ws.user, id) {
			ws.send(map[string]any{"type": "error", "error": "Sin permiso"})
			return
		}
		if data.LastSeenMessageID != nil {
			ws.lastSeenMsgID = data.LastSeenMessageID
		}
		_, _ = rt.db.markConversationPresence(ctx, id, ws.user.Role, ws.lastSeenMsgID)
		full := rt.presencePayload(ctx, id)
		payload := map[string]any{"type": "presence", "conversationId": id, "presence": full}
		ws.send(payload)
		rt.broadcastConversation(id, payload)
	case "typing":
		id := rt.targetConversation(ws, data.ConversationID)
		if !rt.canAccess(ctx, ws.user, id) {
			ws.send(map[string]any{"type": "error", "error": "Sin permiso"})
			return
		}
		isTyping := true
		if data.IsTyping != nil {
			isTyping = *data.IsTyping
		}
		rt.setTypingState(ws, id, isTyping)
		rt.emitTyping(ws, id, isTyping)
	}
}

func (rt *Realtime) targetConversation(ws *wsClient, raw any) int64 {
	if ws.user.Role == "admin" {
		return toInt64(raw)
	}
	if ws.user.ConversationID != nil {
		return *ws.user.ConversationID
	}
	return 0
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// fingerprintFromRequest calcula la huella (user-agent + IP) de la petición del
// upgrade, usando la misma resolución de IP consciente de proxies que el resto
// del servicio.
func (rt *Realtime) fingerprintFromRequest(r *http.Request) fingerprint {
	ua := r.Header.Get("user-agent")
	if ua == "" {
		ua = "unknown"
	}
	ip := ""
	if rt.lim != nil {
		ip = rt.lim.ClientIP(r)
	} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	} else {
		ip = r.RemoteAddr
	}
	return fingerprint{userAgentHash: sha256Hex(ua), ipHash: sha256Hex(ip)}
}

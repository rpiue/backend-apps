package chat

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// --- Sesiones ---

func (d *DB) createChatSession(ctx context.Context, userID int64, role string, app *string, conversationID *int64, email *string, userAgentHash, ipHash string, ipAddress, deviceID *string, ttlHours int) (string, error) {
	jti := newUUID()
	if ttlHours <= 0 {
		ttlHours = 12
	}
	_, err := d.pool.Exec(ctx, `
		insert into chat_sessions (jti, user_id, role, app_name, conversation_id, email, user_agent_hash, ip_hash, ip_address, device_id, expires_at, last_seen_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now() + ($11 * interval '1 hour'), now())`,
		jti, userID, role, app, conversationID, email, userAgentHash, ipHash, ipAddress, deviceID, ttlHours)
	return jti, err
}

func (d *DB) validateChatSession(ctx context.Context, jti string, userID int64, role, userAgentHash, ipHash string) (bool, error) {
	var got string
	err := d.pool.QueryRow(ctx, `
		update chat_sessions set last_seen_at = now()
		where jti = $1 and user_id = $2 and role = $3 and user_agent_hash = $4 and ip_hash = $5
			and revoked_at is null and expires_at > now()
		returning jti`, jti, userID, role, userAgentHash, ipHash).Scan(&got)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (d *DB) revokeChatSession(ctx context.Context, jti string) error {
	_, err := d.pool.Exec(ctx, `update chat_sessions set revoked_at = now() where jti = $1`, jti)
	return err
}

// --- WS tickets ---

func (d *DB) createWsTicket(ctx context.Context, sessionJti string, ttlSeconds int) (string, int, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	ticket := randToken(32)
	_, err := d.pool.Exec(ctx, `
		insert into chat_ws_tickets (ticket_hash, session_jti, expires_at)
		values ($1, $2, now() + ($3 * interval '1 second'))`, sha256Hex(ticket), sessionJti, ttlSeconds)
	return ticket, ttlSeconds, err
}

func (d *DB) consumeWsTicket(ctx context.Context, ticket, userAgentHash, ipHash string) (*SessionRow, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	ticketHash := sha256Hex(ticket)
	var s SessionRow
	err = tx.QueryRow(ctx, `
		select s.jti, s.user_id, s.role, s.app_name, s.conversation_id, s.email
		from chat_ws_tickets t join chat_sessions s on s.jti = t.session_jti
		where t.ticket_hash = $1 and t.consumed_at is null and t.expires_at > now()
			and s.revoked_at is null and s.expires_at > now()
			and s.user_agent_hash = $2 and s.ip_hash = $3
		for update`, ticketHash, userAgentHash, ipHash).
		Scan(&s.JTI, &s.UserID, &s.Role, &s.App, &s.ConversationID, &s.Email)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `update chat_ws_tickets set consumed_at = now() where ticket_hash = $1`, ticketHash); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `update chat_sessions set last_seen_at = now() where jti = $1`, s.JTI); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &s, nil
}

// --- Dedupe de notificaciones ---

func (d *DB) shouldSendNotification(ctx context.Context, dedupeKey string, cooldownMinutes int) (bool, error) {
	var should bool
	err := d.pool.QueryRow(ctx, `
		insert into chat_notification_dedupe (dedupe_key, created_at) values ($1, now())
		on conflict (dedupe_key) do update set created_at = case
			when chat_notification_dedupe.created_at < now() - ($2 * interval '1 minute') then now()
			else chat_notification_dedupe.created_at end
		returning created_at > now() - interval '5 seconds'`, dedupeKey, cooldownMinutes).Scan(&should)
	return should, err
}

func (d *DB) clearNotificationDedupeKey(ctx context.Context, dedupeKey string) error {
	_, err := d.pool.Exec(ctx, `delete from chat_notification_dedupe where dedupe_key = $1`, dedupeKey)
	return err
}

func (d *DB) clearNotificationDedupePrefix(ctx context.Context, prefix string) error {
	_, err := d.pool.Exec(ctx, `delete from chat_notification_dedupe where dedupe_key like $1`, prefix+"%")
	return err
}

// --- Conversaciones (admin) ---

// ConvFilters son los filtros de la lista del panel. Todos se resuelven en SQL:
// aplicarlos en el cliente obligaba a pedir página tras página hasta encontrar
// resultados (con "No leídos" era pulsar "Cargar más" sin parar).
type ConvFilters struct {
	App        string
	Search     string
	UnreadOnly bool
	LabelID    int64
	Limit      int
	Offset     int
}

// unreadExistsSQL es la condición "tiene mensajes del cliente sin ver por el
// admin". Va como EXISTS (no como count > 0) para que corte en el primer acierto
// usando idx_chat_messages_spam_lookup(conversation_id, sender_id, id desc).
const unreadExistsSQL = `exists (
	select 1 from chat_messages m
	where m.conversation_id = c.id and m.sender_id = c.user_id
	  and m.id > coalesce(c.last_admin_seen_message_id, 0))`

// buildConvWhere arma el WHERE de la lista del panel numerando los $N a partir de
// los parámetros que ya trae `params` (limit/offset). Devuelve el WHERE y los
// parámetros en el orden que espera pgx.
func buildConvWhere(f ConvFilters, params []any) (string, []any) {
	filters := ""
	if f.App != "" {
		params = append(params, normalizeApp(f.App))
		filters += fmt.Sprintf(" and c.app_name = $%d", len(params))
	}
	if cleanSearch := lowerTrim(f.Search); cleanSearch != "" {
		params = append(params, "%"+cleanSearch+"%")
		filters += sprintfLike(len(params))
	}
	if f.UnreadOnly {
		filters += " and " + unreadExistsSQL
	}
	if f.LabelID > 0 {
		params = append(params, f.LabelID)
		filters += fmt.Sprintf(` and exists (select 1 from chat_conversation_labels cl
			where cl.conversation_id = c.id and cl.label_id = $%d)`, len(params))
	}
	if filters == "" {
		return "", params
	}
	return "where 1=1" + filters, params
}

func (d *DB) listConversationsForAdmin(ctx context.Context, f ConvFilters) ([]AdminConversation, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 30
	} else if limit > 100 {
		limit = 100
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// Cache con TTL corto: a miles de chats esta consulta (con subqueries de
	// último mensaje y no-leídos por fila) es la más cara. El "online" se añade
	// fresco fuera de aquí, así que no se cachea estado de presencia.
	cacheKey := ""
	if d.cache != nil {
		cacheKey = "chat:cl:" + normalizeApp(f.App) + ":" + lowerTrim(f.Search) +
			":" + strconv.FormatBool(f.UnreadOnly) + ":" + strconv.FormatInt(f.LabelID, 10) +
			":" + strconv.Itoa(limit) + ":" + strconv.Itoa(offset)
		var cached []AdminConversation
		if ok, _ := d.cache.GetJSON(ctx, cacheKey, &cached); ok {
			return cached, nil
		}
	}

	where, params := buildConvWhere(f, []any{limit, offset})
	// El orden lleva c.id como desempate para que la paginación sea estable:
	// con updated_at repetido, el offset podía repetir o saltarse filas.
	q := `
		select c.id, c.app_name, c.status, c.updated_at, c.last_user_presence_at,
			u.id, u.email, u.nombre, u.apellidos, u.telefono, u.guest_number,
			(select body_ciphertext from chat_messages m where m.conversation_id = c.id order by m.id desc limit 1),
			(select count(*)::int from chat_messages m where m.conversation_id = c.id and m.sender_id = c.user_id and m.id > coalesce(c.last_admin_seen_message_id, 0))
		from chat_conversations c
		join chat_users u on u.id = c.user_id
		` + where + `
		order by c.updated_at desc, c.id desc limit $1 offset $2`
	rows, err := d.pool.Query(ctx, q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminConversation{}
	for rows.Next() {
		var c AdminConversation
		var guest *int64
		if err := rows.Scan(&c.ID, &c.App, &c.Status, &c.UpdatedAt, &c.LastUserPresenceAt,
			&c.User.ID, &c.User.Email, &c.User.Nombre, &c.User.Apellidos, &c.User.Telefono, &guest,
			&c.LastMessage, &c.UnreadCount); err != nil {
			return nil, err
		}
		c.User.GuestNumber = guest
		c.User.IsGuest = guest != nil
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cacheKey != "" {
		_ = d.cache.SetJSON(ctx, cacheKey, out, adminListTTL)
	}
	return out, nil
}

// countUnreadConversations cuenta TODAS las conversaciones con mensajes sin leer,
// no solo las de la página cargada: el badge de "No leídos" se quedaba corto
// porque se calculaba sobre la lista que el panel tenía en memoria.
func (d *DB) countUnreadConversations(ctx context.Context, app string) (int, error) {
	cacheKey := ""
	if d.cache != nil {
		cacheKey = "chat:unreadtotal:" + normalizeApp(app)
		var cached int
		if ok, _ := d.cache.GetJSON(ctx, cacheKey, &cached); ok {
			return cached, nil
		}
	}
	params := []any{}
	where := "where " + unreadExistsSQL
	if app != "" {
		params = append(params, normalizeApp(app))
		where += " and c.app_name = $1"
	}
	var n int
	err := d.pool.QueryRow(ctx, `select count(*)::int from chat_conversations c `+where, params...).Scan(&n)
	if err != nil {
		return 0, err
	}
	if cacheKey != "" {
		_ = d.cache.SetJSON(ctx, cacheKey, n, adminListTTL)
	}
	return n, nil
}

func (d *DB) deleteConversationHard(ctx context.Context, conversationID int64) (bool, error) {
	var id int64
	err := d.pool.QueryRow(ctx, `delete from chat_conversations where id = $1 returning id`, conversationID).Scan(&id)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (d *DB) deleteAllConversationsHard(ctx context.Context) ([]int64, error) {
	rows, err := d.pool.Query(ctx, `delete from chat_conversations returning id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

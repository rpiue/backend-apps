package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"codex/backend/internal/cache"
)

// DB envuelve el pool de Postgres con todas las consultas del chat (port de db.js).
// cache (Redis) es opcional: si está, cachea las consultas pesadas (lista de
// conversaciones del admin y páginas de mensajes) con invalidación por versión.
type DB struct {
	pool  *pgxpool.Pool
	cache *cache.Cache
}

func newDB(pool *pgxpool.Pool, c *cache.Cache) *DB { return &DB{pool: pool, cache: c} }

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func normalizeEmail(email string) string {
	s := strings.TrimSpace(email)
	s = strings.Join(strings.Fields(s), "") // quita TODOS los espacios internos
	return strings.ToLower(s)
}

func normalizeApp(app string) string {
	a := strings.ToLower(strings.TrimSpace(app))
	switch a {
	case "bcp":
		return "bcp"
	case "interbank":
		return "interbank"
	default:
		return "yape"
	}
}

// upsertChatUser replica upsertChatUser.
func (d *DB) upsertChatUser(ctx context.Context, app, email string, firebaseUserID, nombre, apellidos, telefono *string, role string) (*PublicUser, error) {
	cleanEmail := normalizeEmail(email)
	cleanApp := normalizeApp(app)
	if role == "" {
		role = "user"
	}
	row := d.pool.QueryRow(ctx, `
		insert into chat_users (app_name, email, email_hash, firebase_user_id, nombre, apellidos, telefono, role, last_seen_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, now())
		on conflict (app_name, email_hash)
		do update set
			email = excluded.email,
			firebase_user_id = coalesce(excluded.firebase_user_id, chat_users.firebase_user_id),
			nombre = coalesce(excluded.nombre, chat_users.nombre),
			apellidos = coalesce(excluded.apellidos, chat_users.apellidos),
			telefono = coalesce(excluded.telefono, chat_users.telefono),
			role = excluded.role,
			last_seen_at = now(),
			updated_at = now()
		returning id, app_name, email, firebase_user_id, nombre, apellidos, telefono, guest_number, role`,
		cleanApp, cleanEmail, sha256Hex(cleanEmail), firebaseUserID, nombre, apellidos, telefono, role)
	return scanPublicUser(row)
}

func scanPublicUser(row pgx.Row) (*PublicUser, error) {
	var u PublicUser
	var guest *int64
	err := row.Scan(&u.ID, &u.App, &u.Email, &u.FirebaseUserID, &u.Nombre, &u.Apellidos, &u.Telefono, &guest, &u.Role)
	if err != nil {
		return nil, err
	}
	u.GuestNumber = guest
	u.IsGuest = guest != nil
	return &u, nil
}

// ensureGuestUserByIp replica ensureGuestUserByIp.
func (d *DB) ensureGuestUserByIp(ctx context.Context, app, ipHash string) (*PublicUser, error) {
	cleanApp := normalizeApp(app)
	ipPart := ipHash
	if len(ipPart) > 32 {
		ipPart = ipPart[:32]
	}
	guestEmail := fmt.Sprintf("guest+%s+%s@guest.local", cleanApp, ipPart)
	row := d.pool.QueryRow(ctx, `
		insert into chat_users (app_name, email, email_hash, nombre, guest_number, role, last_seen_at)
		values ($1, $2, $3, null, nextval('chat_guest_number_seq'), 'user', now())
		on conflict (app_name, email_hash)
		do update set last_seen_at = now(), updated_at = now()
		returning id, app_name, email, firebase_user_id, nombre, apellidos, telefono, guest_number, role`,
		cleanApp, guestEmail, sha256Hex(guestEmail))
	u, err := scanPublicUser(row)
	if err != nil {
		return nil, err
	}
	if u.Nombre != nil && *u.Nombre != "" {
		return u, nil
	}
	name := "Invitado"
	if u.GuestNumber != nil {
		name = fmt.Sprintf("Invitado %d", *u.GuestNumber)
	}
	row = d.pool.QueryRow(ctx, `
		update chat_users set nombre = $2, updated_at = now() where id = $1
		returning id, app_name, email, firebase_user_id, nombre, apellidos, telefono, guest_number, role`,
		u.ID, name)
	return scanPublicUser(row)
}

func (d *DB) getAdminUser(ctx context.Context) (*PublicUser, error) {
	row := d.pool.QueryRow(ctx, `
		select id, app_name, email, firebase_user_id, nombre, apellidos, telefono, guest_number, role
		from chat_users where role = 'admin' order by created_at asc limit 1`)
	u, err := scanPublicUser(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// ensureConversation replica ensureConversation.
func (d *DB) ensureConversation(ctx context.Context, app string, userID int64) (*Conversation, error) {
	admin, err := d.getAdminUser(ctx)
	if err != nil {
		return nil, err
	}
	if admin == nil {
		return nil, fmt.Errorf("Admin de chat no inicializado.")
	}
	cleanApp := normalizeApp(app)
	var c Conversation
	err = d.pool.QueryRow(ctx, `
		insert into chat_conversations (app_name, user_id, admin_id)
		values ($1, $2, $3)
		on conflict (app_name, user_id, admin_id)
		do update set updated_at = now()
		returning id, app_name, user_id, admin_id`,
		cleanApp, userID, admin.ID).Scan(&c.ID, &c.App, &c.UserID, &c.AdminID)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// listMessages replica listMessages con reacciones y replyTo.
func (d *DB) listMessages(ctx context.Context, conversationID int64, afterID, beforeID *int64, limit int) ([]Message, error) {
	safeLimit := limit
	if safeLimit <= 0 {
		safeLimit = 50
	}
	if safeLimit > 80 {
		safeLimit = 80
	}

	// Cache por versión de conversación (invalidada en cada escritura).
	cacheKey := ""
	if d.cache != nil {
		cacheKey = "chat:msgs:" + strconv.FormatInt(conversationID, 10) + ":" +
			strconv.FormatInt(d.convVer(ctx, conversationID), 10) + ":a" + ptrKey(afterID) +
			":b" + ptrKey(beforeID) + ":l" + strconv.Itoa(safeLimit)
		var cached []Message
		if ok, _ := d.cache.GetJSON(ctx, cacheKey, &cached); ok {
			return cached, nil
		}
	}

	params := []any{conversationID, safeLimit}
	pageSQL := ""
	order := "desc"
	if afterID != nil {
		params = append(params, *afterID)
		pageSQL = "and m.id > $3"
		order = "asc"
	} else if beforeID != nil {
		params = append(params, *beforeID)
		pageSQL = "and m.id < $3"
	}

	q := fmt.Sprintf(`
		with page as (
			select m.id, m.conversation_id, m.sender_id, m.reply_to_message_id, m.kind,
				m.body_ciphertext, m.body_iv, m.body_tag, m.client_nonce, m.created_at
			from chat_messages m
			where m.conversation_id = $1 %s
			order by m.id %s
			limit $2
		)
		select
			m.id, m.conversation_id, m.sender_id, u.role as sender_role,
			m.reply_to_message_id, m.kind, m.body_ciphertext, m.body_iv, m.body_tag, m.client_nonce, m.created_at,
			a.id, a.original_name, a.mime_type, a.size_bytes, a.width, a.height, a.duration_ms, a.scan_status, a.scan_signature,
			rm.id, rm.sender_id, ru.role, rm.kind, rm.body_ciphertext, rm.body_iv, rm.body_tag, rm.created_at,
			ra.id, ra.original_name, ra.mime_type, ra.size_bytes,
			coalesce(
				(select json_agg(json_build_object('emoji', g.emoji, 'count', g.count, 'userIds', g.user_ids) order by g.count desc, g.emoji)
				 from (select r.emoji, count(*)::int as count, array_agg(r.user_id order by r.user_id) as user_ids
				       from chat_message_reactions r where r.message_id = m.id group by r.emoji) g),
				'[]'::json) as reactions
		from page m
		join chat_users u on u.id = m.sender_id
		left join chat_attachments a on a.message_id = m.id
		left join chat_messages rm on rm.id = m.reply_to_message_id and rm.conversation_id = m.conversation_id
		left join chat_users ru on ru.id = rm.sender_id
		left join chat_attachments ra on ra.message_id = rm.id
		order by m.id asc`, pageSQL, order)

	rows, err := d.pool.Query(ctx, q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var m Message
		var aID *int64
		var aName *string
		var aMime *string
		var aSize *int64
		var aWidth, aHeight, aDur *int
		var aScanStatus, aScanSig *string
		var rID, rSender *int64
		var rRole, rKind, rCipher, rIV, rTag *string
		var rCreated *time.Time
		var raID *int64
		var raName, raMime *string
		var raSize *int64
		var reactionsJSON []byte
		if err := rows.Scan(
			&m.ID, &m.ConversationID, &m.SenderID, &m.SenderRole,
			&m.ReplyToMessageID, &m.Kind, &m.Ciphertext, &m.IV, &m.Tag, &m.ClientNonce, &m.CreatedAt,
			&aID, &aName, &aMime, &aSize, &aWidth, &aHeight, &aDur, &aScanStatus, &aScanSig,
			&rID, &rSender, &rRole, &rKind, &rCipher, &rIV, &rTag, &rCreated,
			&raID, &raName, &raMime, &raSize,
			&reactionsJSON,
		); err != nil {
			return nil, err
		}
		if aID != nil {
			m.Attachment = &Attachment{ID: *aID, Name: aName, MimeType: derefStr(aMime), Size: derefInt64(aSize), Width: aWidth, Height: aHeight, DurationMs: aDur, ScanStatus: derefStr(aScanStatus), ScanSignature: derefStr(aScanSig)}
		}
		if rID != nil {
			cm := &CompactMessage{ID: *rID, SenderID: derefInt64(rSender), SenderRole: derefStr(rRole), Kind: derefStr(rKind), Ciphertext: derefStr(rCipher), IV: derefStr(rIV), Tag: derefStr(rTag)}
			if rCreated != nil {
				cm.CreatedAt = *rCreated
			}
			if raID != nil {
				cm.Attachment = &Attachment{ID: *raID, Name: raName, MimeType: derefStr(raMime), Size: derefInt64(raSize)}
			}
			m.ReplyTo = cm
		}
		m.Reactions = parseReactions(reactionsJSON)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cacheKey != "" {
		_ = d.cache.SetJSON(ctx, cacheKey, out, msgPageTTL)
	}
	return out, nil
}

// createMessage replica createMessage (transacción + validación replyTo).
func (d *DB) createMessage(ctx context.Context, conversationID, senderID int64, kind string, enc Encrypted, clientNonce string, spamFingerprint, spamSignature *string, replyTo *int64) (*rawMessage, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var cleanReply *int64
	if replyTo != nil && *replyTo > 0 {
		var id int64
		e := tx.QueryRow(ctx, `select id from chat_messages where id = $1 and conversation_id = $2 limit 1`, *replyTo, conversationID).Scan(&id)
		if e == nil {
			cleanReply = &id
		}
	}
	if clientNonce == "" {
		clientNonce = newUUID()
	}
	var r rawMessage
	err = tx.QueryRow(ctx, `
		insert into chat_messages (conversation_id, sender_id, reply_to_message_id, kind, body_ciphertext, body_iv, body_tag, client_nonce, spam_fingerprint, spam_signature)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		returning id, conversation_id, sender_id, kind`,
		conversationID, senderID, cleanReply, kind, enc.Ciphertext, enc.IV, enc.Tag, clientNonce, spamFingerprint, spamSignature).
		Scan(&r.ID, &r.ConversationID, &r.SenderID, &r.Kind)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `update chat_conversations set updated_at = now() where id = $1`, conversationID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	d.bumpConv(ctx, conversationID) // invalida páginas cacheadas
	return &r, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

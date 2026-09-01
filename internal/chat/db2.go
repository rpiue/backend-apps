package chat

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// getConversationUserSendState replica getConversationUserSendState (anti-spam).
func (d *DB) getConversationUserSendState(ctx context.Context, conversationID, userID int64, spamFingerprint *string) (*SendState, error) {
	var pending int
	if err := d.pool.QueryRow(ctx, `
		with last_admin as (
			select coalesce(max(m.id), 0) as last_admin_message_id
			from chat_messages m join chat_users u on u.id = m.sender_id
			where m.conversation_id = $1 and u.role = 'admin'
		)
		select count(*)::int
		from chat_messages m, last_admin
		where m.conversation_id = $1 and m.sender_id = $2
			and m.id > last_admin.last_admin_message_id and m.deleted_at is null`,
		conversationID, userID).Scan(&pending); err != nil {
		return nil, err
	}

	rows, err := d.pool.Query(ctx, `
		with last_admin as (
			select coalesce(max(m.id), 0) as last_admin_message_id
			from chat_messages m join chat_users u on u.id = m.sender_id
			where m.conversation_id = $1 and u.role = 'admin'
		)
		select id, spam_fingerprint, spam_signature, created_at
		from chat_messages m, last_admin
		where m.conversation_id = $1 and m.sender_id = $2
			and m.id > last_admin.last_admin_message_id and m.deleted_at is null
			and m.spam_fingerprint is not null
		order by id desc limit 100`, conversationID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	st := &SendState{PendingUserMessages: pending}
	for rows.Next() {
		var r recentMsg
		if err := rows.Scan(&r.ID, &r.SpamFinger, &r.SpamSignature, &r.CreatedAt); err != nil {
			return nil, err
		}
		st.Recent = append(st.Recent, r)
	}
	if spamFingerprint != nil {
		for _, r := range st.Recent {
			if r.SpamFinger != nil && *r.SpamFinger == *spamFingerprint && r.CreatedAt.After(time.Now().Add(-5*time.Minute)) {
				st.DuplicateRecent = true
				break
			}
		}
	}
	return st, rows.Err()
}

// shouldSendAutomationReply replica el upsert con cooldown de chat_automation_dedupe.
func (d *DB) shouldSendAutomationReply(ctx context.Context, conversationID, automationID int64, questionHash string, cooldownMinutes int) (bool, error) {
	var should bool
	err := d.pool.QueryRow(ctx, `
		with existing as (
			select last_sent_at from chat_automation_dedupe
			where conversation_id = $1 and automation_id = $2 and question_hash = $3
		),
		upserted as (
			insert into chat_automation_dedupe (conversation_id, automation_id, question_hash, last_sent_at)
			values ($1, $2, $3, now())
			on conflict (conversation_id, automation_id, question_hash) do update set last_sent_at = now()
			returning 1
		)
		select coalesce((select last_sent_at < now() - ($4 * interval '1 minute') from existing), true)`,
		conversationID, automationID, questionHash, cooldownMinutes).Scan(&should)
	return should, err
}

// getMessage devuelve {id, conversation_id} o nil.
func (d *DB) getMessage(ctx context.Context, messageID int64) (*rawMessage, error) {
	var r rawMessage
	err := d.pool.QueryRow(ctx, `select id, conversation_id from chat_messages where id = $1 limit 1`, messageID).
		Scan(&r.ID, &r.ConversationID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

func (d *DB) getMessageReactions(ctx context.Context, messageID int64) ([]Reaction, error) {
	rows, err := d.pool.Query(ctx, `
		select emoji, count(*)::int, array_agg(user_id order by user_id)
		from chat_message_reactions where message_id = $1
		group by emoji order by count(*) desc, emoji`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reaction{}
	for rows.Next() {
		var r Reaction
		if err := rows.Scan(&r.Emoji, &r.Count, &r.UserIDs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) setMessageReaction(ctx context.Context, messageID, userID int64, emoji string) ([]Reaction, error) {
	if _, err := d.pool.Exec(ctx, `
		insert into chat_message_reactions (message_id, user_id, emoji) values ($1,$2,$3)
		on conflict (message_id, user_id, emoji) do nothing`, messageID, userID, emoji); err != nil {
		return nil, err
	}
	d.bumpConvByMessage(ctx, messageID)
	return d.getMessageReactions(ctx, messageID)
}

func (d *DB) deleteMessageReaction(ctx context.Context, messageID, userID int64, emoji string) ([]Reaction, error) {
	if _, err := d.pool.Exec(ctx, `delete from chat_message_reactions where message_id=$1 and user_id=$2 and emoji=$3`, messageID, userID, emoji); err != nil {
		return nil, err
	}
	d.bumpConvByMessage(ctx, messageID)
	return d.getMessageReactions(ctx, messageID)
}

// bumpConvByMessage invalida la cache de la conversación dueña del mensaje.
func (d *DB) bumpConvByMessage(ctx context.Context, messageID int64) {
	if d.cache == nil {
		return
	}
	if m, _ := d.getMessage(ctx, messageID); m != nil {
		d.bumpConv(ctx, m.ConversationID)
	}
}

// fileMeta son los datos del archivo a persistir como adjunto.
type fileMeta struct {
	Path            string
	OriginalName    string
	MimeType        string
	Size            int64
	SHA256          string
	DurationMs      *int
	StorageEncoding string
	ScanStatus      string // clean | infected | error | unscanned
	ScanSignature   string // firma de ClamAV si infected
}

func (d *DB) createAttachment(ctx context.Context, messageID, uploaderID int64, f fileMeta) error {
	enc := f.StorageEncoding
	if enc == "" {
		enc = "identity"
	}
	status := f.ScanStatus
	if status == "" {
		status = "unscanned"
	}
	var sig *string
	if f.ScanSignature != "" {
		sig = &f.ScanSignature
	}
	_, err := d.pool.Exec(ctx, `
		insert into chat_attachments (message_id, uploader_id, storage_path, original_name, mime_type, size_bytes, sha256, duration_ms, storage_encoding, scan_status, scan_signature)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		messageID, uploaderID, f.Path, f.OriginalName, f.MimeType, f.Size, f.SHA256, f.DurationMs, enc, status, sig)
	return err
}

func (d *DB) getAttachment(ctx context.Context, attachmentID int64) (*AttachmentRow, error) {
	var a AttachmentRow
	err := d.pool.QueryRow(ctx, `
		select a.id, a.message_id, m.conversation_id, a.storage_path, a.original_name, a.mime_type, a.size_bytes, a.storage_encoding
		from chat_attachments a join chat_messages m on m.id = a.message_id
		where a.id = $1`, attachmentID).
		Scan(&a.ID, &a.MessageID, &a.ConversationID, &a.StoragePath, &a.OriginalName, &a.MimeType, &a.SizeBytes, &a.StorageEncoding)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

// countUnseenAdminMessages cuenta los mensajes del soporte (admin) que el
// usuario aún no ha visto en una conversación (id > last_user_seen).
func (d *DB) countUnseenAdminMessages(ctx context.Context, conversationID int64) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		select count(*) from chat_messages m
		join chat_conversations c on c.id = m.conversation_id
		where m.conversation_id = $1
		  and m.sender_id = c.admin_id
		  and m.id > coalesce(c.last_user_seen_message_id, 0)`, conversationID).Scan(&n)
	return n, err
}

func (d *DB) getConversationMember(ctx context.Context, conversationID, userID int64) (bool, error) {
	var id int64
	err := d.pool.QueryRow(ctx, `select id from chat_conversations where id = $1 and (user_id = $2 or admin_id = $2)`, conversationID, userID).Scan(&id)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// verifyAdmin valida email+password usando crypt (bcrypt en la DB) -> compatible
// con los hashes existentes en producción.
func (d *DB) verifyAdmin(ctx context.Context, email, password string) (*PublicUser, error) {
	cleanEmail := normalizeEmail(email)
	row := d.pool.QueryRow(ctx, `
		select id, app_name, email, firebase_user_id, nombre, apellidos, telefono, guest_number, role
		from chat_users
		where role = 'admin' and email_hash = $1 and password_hash = crypt($2, password_hash)
		limit 1`, sha256Hex(cleanEmail), password)
	u, err := scanPublicUser(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (d *DB) getConversation(ctx context.Context, conversationID int64) (*ConversationDetails, error) {
	var c ConversationDetails
	err := d.pool.QueryRow(ctx, `
		select id, app_name, user_id, admin_id, status
		from chat_conversations where id = $1 limit 1`, conversationID).
		Scan(&c.ID, &c.AppName, &c.UserID, &c.AdminID, &c.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

func (d *DB) getConversationDetails(ctx context.Context, conversationID int64) (*ConversationDetails, error) {
	var c ConversationDetails
	err := d.pool.QueryRow(ctx, `
		select c.id, c.app_name, c.user_id, c.admin_id, c.status, c.created_at,
			c.last_user_presence_at, c.last_user_seen_message_id, c.last_admin_seen_message_id,
			u.email, u.nombre, u.apellidos, u.guest_number, a.email
		from chat_conversations c
		join chat_users u on u.id = c.user_id
		join chat_users a on a.id = c.admin_id
		where c.id = $1 limit 1`, conversationID).
		Scan(&c.ID, &c.AppName, &c.UserID, &c.AdminID, &c.Status, &c.CreatedAt,
			&c.LastUserPresenceAt, &c.LastUserSeenMessageID, &c.LastAdminSeenMessageID,
			&c.UserEmail, &c.UserNombre, &c.UserApellidos, &c.UserGuestNumber, &c.AdminEmail)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

// getUserDeviceStats cuenta dispositivos/sesiones distintas de un usuario del
// chat (para el panel de info): cuántos equipos ha usado, primera y última
// conexión, y cuántas sesiones siguen vivas.
func (d *DB) getUserDeviceStats(ctx context.Context, userID int64) (*UserDeviceStats, error) {
	var s UserDeviceStats
	err := d.pool.QueryRow(ctx, `
		select
			count(distinct device_id) filter (where device_id is not null and device_id <> ''),
			count(*),
			min(created_at),
			max(coalesce(last_seen_at, created_at)),
			count(*) filter (where revoked_at is null and expires_at > now())
		from chat_sessions
		where user_id = $1 and role = 'user'`, userID).
		Scan(&s.DeviceCount, &s.SessionCount, &s.FirstSeenAt, &s.LastSeenAt, &s.ActiveSessions)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (d *DB) markConversationPresence(ctx context.Context, conversationID int64, role string, lastSeenMessageID *int64) (*Presence, error) {
	isAdmin := role == "admin"
	presenceCol := "last_user_presence_at"
	seenCol := "last_user_seen_message_id"
	if isAdmin {
		presenceCol = "last_admin_presence_at"
		seenCol = "last_admin_seen_message_id"
	}
	params := []any{conversationID}
	seenSQL := ""
	if lastSeenMessageID != nil {
		params = append(params, *lastSeenMessageID)
		seenSQL = ", " + seenCol + " = greatest(coalesce(" + seenCol + ", 0), $2)"
	}
	_, err := d.pool.Exec(ctx, "update chat_conversations set "+presenceCol+" = now()"+seenSQL+" where id = $1", params...)
	if err != nil {
		return nil, err
	}
	return d.getConversationPresence(ctx, conversationID)
}

func (d *DB) getConversationPresence(ctx context.Context, conversationID int64) (*Presence, error) {
	var p Presence
	err := d.pool.QueryRow(ctx, `
		select id, last_user_presence_at, last_admin_presence_at, last_user_seen_message_id, last_admin_seen_message_id,
			(last_user_presence_at > now() - interval '35 seconds'),
			(last_admin_presence_at > now() - interval '35 seconds')
		from chat_conversations where id = $1 limit 1`, conversationID).
		Scan(&p.ID, &p.LastUserPresenceAt, &p.LastAdminPresenceAt, &p.LastUserSeenMessageID, &p.LastAdminSeenMessageID, &p.UserOnline, &p.AdminOnline)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

func (d *DB) getStorageUsage(ctx context.Context, userID int64) (int64, error) {
	var used int64
	err := d.pool.QueryRow(ctx, `select coalesce(sum(size_bytes), 0)::bigint from chat_attachments where uploader_id = $1`, userID).Scan(&used)
	return used, err
}

func (d *DB) getLatestUserSessionInfo(ctx context.Context, userID int64) (*UserSessionInfo, error) {
	var s UserSessionInfo
	err := d.pool.QueryRow(ctx, `
		select ip_address, device_id, last_seen_at, created_at, expires_at, revoked_at
		from chat_sessions where user_id = $1 and role = 'user'
		order by coalesce(last_seen_at, created_at) desc, created_at desc limit 1`, userID).
		Scan(&s.IPAddress, &s.DeviceID, &s.LastSeenAt, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &s, err
}

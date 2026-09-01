package chat

import (
	"context"
	"log"
	"time"
)

// recordMalwareEvent registra (best-effort) que un usuario envió un adjunto
// detectado como malware, para la analítica del panel. No bloquea el flujo.
func (s *Service) recordMalwareEvent(ctx context.Context, conversationID, uploaderID int64, role, kind, mime, signature, sha string) {
	var sig, sh *string
	if signature != "" {
		sig = &signature
	}
	if sha != "" {
		sh = &sha
	}
	_, err := s.db.pool.Exec(ctx, `
		insert into chat_malware_events (conversation_id, uploader_id, uploader_role, kind, mime_type, signature, sha256)
		values ($1,$2,$3,$4,$5,$6,$7)`,
		conversationID, uploaderID, role, kind, mime, sig, sh)
	if err != nil {
		log.Printf("[chat] no se pudo registrar evento de malware: %v", err)
	}
}

// MalwareEvent es un evento de malware para el panel admin.
type MalwareEvent struct {
	ID             int64     `json:"id"`
	ConversationID *int64    `json:"conversationId"`
	UploaderID     *int64    `json:"uploaderId"`
	UploaderRole   string    `json:"uploaderRole"`
	Kind           string    `json:"kind"`
	MimeType       string    `json:"mimeType"`
	Signature      string    `json:"signature"`
	SHA256         string    `json:"sha256"`
	CreatedAt      time.Time `json:"createdAt"`
}

// MalwareByUser agrega el conteo de malware por usuario.
type MalwareByUser struct {
	UploaderID *int64    `json:"uploaderId"`
	Nombre     string    `json:"nombre"`
	Email      string    `json:"email"`
	Count      int       `json:"count"`
	LastAt     time.Time `json:"lastAt"`
}

// MalwareStats es el resumen para el panel.
type MalwareStats struct {
	Total       int             `json:"total"`
	Last30Days  int             `json:"last30Days"`
	BySignature map[string]int  `json:"bySignature"`
	TopUsers    []MalwareByUser `json:"topUsers"`
	Recent      []MalwareEvent  `json:"recent"`
}

// malwareStats calcula las métricas de malware para el panel admin.
func (d *DB) malwareStats(ctx context.Context, limit int) (*MalwareStats, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := &MalwareStats{BySignature: map[string]int{}}

	_ = d.pool.QueryRow(ctx, `select count(*) from chat_malware_events`).Scan(&out.Total)
	_ = d.pool.QueryRow(ctx,
		`select count(*) from chat_malware_events where created_at > now() - interval '30 days'`).
		Scan(&out.Last30Days)

	// Por firma.
	if rows, err := d.pool.Query(ctx,
		`select coalesce(signature,'(desconocida)') sig, count(*) c from chat_malware_events group by sig order by c desc limit 50`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var sig string
			var c int
			if err := rows.Scan(&sig, &c); err == nil {
				out.BySignature[sig] = c
			}
		}
	}

	// Top usuarios.
	if rows, err := d.pool.Query(ctx, `
		select e.uploader_id, coalesce(u.nombre,''), coalesce(u.email,''), count(*) c, max(e.created_at)
		from chat_malware_events e left join chat_users u on u.id = e.uploader_id
		group by e.uploader_id, u.nombre, u.email order by c desc limit 20`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var mu MalwareByUser
			if err := rows.Scan(&mu.UploaderID, &mu.Nombre, &mu.Email, &mu.Count, &mu.LastAt); err == nil {
				out.TopUsers = append(out.TopUsers, mu)
			}
		}
	}

	// Eventos recientes.
	if rows, err := d.pool.Query(ctx, `
		select id, conversation_id, uploader_id, coalesce(uploader_role,''), coalesce(kind,''),
		       coalesce(mime_type,''), coalesce(signature,''), coalesce(sha256,''), created_at
		from chat_malware_events order by created_at desc limit $1`, limit); err == nil {
		defer rows.Close()
		for rows.Next() {
			var ev MalwareEvent
			if err := rows.Scan(&ev.ID, &ev.ConversationID, &ev.UploaderID, &ev.UploaderRole,
				&ev.Kind, &ev.MimeType, &ev.Signature, &ev.SHA256, &ev.CreatedAt); err == nil {
				out.Recent = append(out.Recent, ev)
			}
		}
	}
	return out, nil
}

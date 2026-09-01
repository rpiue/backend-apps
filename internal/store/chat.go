package store

import (
	"context"
	"time"
)

// Conversacion y Mensaje modelan el chat estilo WhatsApp (admin <-> usuarios).
type Conversacion struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	App           string    `json:"app"`
	Nombre        string    `json:"nombre"`
	UltimoMsg     string    `json:"ultimoMsg"`
	NoLeidos      int       `json:"noLeidos"`
	NecesitaAdmin bool      `json:"necesitaAdmin"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type Mensaje struct {
	ID        int64     `json:"id"`
	ConvID    string    `json:"convId"`
	Autor     string    `json:"autor"` // "admin" | "user"
	Cuerpo    string    `json:"cuerpo"`
	Leido     bool      `json:"leido"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListConversaciones devuelve las conversaciones ordenadas por actividad.
func (s *Store) ListConversaciones(ctx context.Context) ([]Conversacion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, app, COALESCE(nombre,''), COALESCE(ultimo_msg,''), no_leidos, necesita_admin, updated_at
		FROM chat_conversacion ORDER BY necesita_admin DESC, updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversacion{}
	for rows.Next() {
		var c Conversacion
		if err := rows.Scan(&c.ID, &c.Email, &c.App, &c.Nombre, &c.UltimoMsg, &c.NoLeidos, &c.NecesitaAdmin, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetMensajes devuelve los mensajes de una conversación (más recientes al final).
func (s *Store) GetMensajes(ctx context.Context, convID string, limit int) ([]Mensaje, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, conv_id, autor, cuerpo, leido, created_at
		FROM chat_mensaje WHERE conv_id=$1 ORDER BY created_at ASC LIMIT $2`, convID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Mensaje{}
	for rows.Next() {
		var m Mensaje
		if err := rows.Scan(&m.ID, &m.ConvID, &m.Autor, &m.Cuerpo, &m.Leido, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// EnsureConversacion crea (o recupera) la conversación de un usuario por (email, app).
func (s *Store) EnsureConversacion(ctx context.Context, email, app, nombre string) (string, error) {
	if app == "" {
		app = "yape"
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO chat_conversacion (id, email, app, nombre)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (email, app) DO UPDATE SET nombre = COALESCE(NULLIF(EXCLUDED.nombre,''), chat_conversacion.nombre)
		RETURNING id`, newID(), email, app, nombre).Scan(&id)
	return id, err
}

// InsertMensaje guarda un mensaje y actualiza el resumen de la conversación.
func (s *Store) InsertMensaje(ctx context.Context, convID, autor, cuerpo string) (Mensaje, error) {
	var m Mensaje
	err := s.pool.QueryRow(ctx, `
		INSERT INTO chat_mensaje (conv_id, autor, cuerpo) VALUES ($1,$2,$3)
		RETURNING id, conv_id, autor, cuerpo, leido, created_at`,
		convID, autor, cuerpo).Scan(&m.ID, &m.ConvID, &m.Autor, &m.Cuerpo, &m.Leido, &m.CreatedAt)
	if err != nil {
		return m, err
	}
	// Si lo manda el usuario, incrementa no leídos; si lo manda el admin, no.
	inc := 0
	if autor == "user" {
		inc = 1
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE chat_conversacion SET ultimo_msg=$1, updated_at=now(), no_leidos = no_leidos + $2
		WHERE id=$3`, cuerpo, inc, convID)
	return m, err
}

// MarcarLeidos pone en 0 los no leídos de una conversación.
func (s *Store) MarcarLeidos(ctx context.Context, convID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_conversacion SET no_leidos=0 WHERE id=$1`, convID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE chat_mensaje SET leido=true WHERE conv_id=$1 AND autor='user'`, convID)
	return err
}

// SetNecesitaAdmin marca/desmarca una conversación como "necesita atención humana".
func (s *Store) SetNecesitaAdmin(ctx context.Context, convID string, v bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_conversacion SET necesita_admin=$1 WHERE id=$2`, v, convID)
	return err
}

// ConversacionByID recupera una conversación por id (para notificar al usuario).
func (s *Store) ConversacionByID(ctx context.Context, id string) (Conversacion, error) {
	var c Conversacion
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, app, COALESCE(nombre,''), COALESCE(ultimo_msg,''), no_leidos, necesita_admin, updated_at
		FROM chat_conversacion WHERE id=$1`, id).Scan(&c.ID, &c.Email, &c.App, &c.Nombre, &c.UltimoMsg, &c.NoLeidos, &c.NecesitaAdmin, &c.UpdatedAt)
	return c, err
}

// --- Stats para el dashboard ---

type Stats struct {
	Suscriptores     int `json:"suscriptores"`
	ComprasPend      int `json:"comprasPendientes"`
	Conversaciones   int `json:"conversaciones"`
	MensajesNoLeidos int `json:"mensajesNoLeidos"`
}

func (s *Store) GetStats(ctx context.Context) (Stats, error) {
	var st Stats
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM suscriptor WHERE activo=true`).Scan(&st.Suscriptores)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM compra WHERE estado='pendiente'`).Scan(&st.ComprasPend)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM chat_conversacion`).Scan(&st.Conversaciones)
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(no_leidos),0) FROM chat_conversacion`).Scan(&st.MensajesNoLeidos)
	return st, nil
}

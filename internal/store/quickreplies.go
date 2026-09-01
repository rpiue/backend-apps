package store

import "context"

// RespuestaRapida es una respuesta predefinida configurable para el chat.
type RespuestaRapida struct {
	ID    string `json:"id"`
	Texto string `json:"texto"`
	Orden int    `json:"orden"`
}

func (s *Store) ListRespuestas(ctx context.Context) ([]RespuestaRapida, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, texto, orden FROM respuesta_rapida ORDER BY orden, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RespuestaRapida{}
	for rows.Next() {
		var r RespuestaRapida
		if err := rows.Scan(&r.ID, &r.Texto, &r.Orden); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CreateRespuesta(ctx context.Context, texto string, orden int) (RespuestaRapida, error) {
	r := RespuestaRapida{ID: newID(), Texto: texto, Orden: orden}
	_, err := s.pool.Exec(ctx, `INSERT INTO respuesta_rapida (id, texto, orden) VALUES ($1,$2,$3)`, r.ID, r.Texto, r.Orden)
	return r, err
}

func (s *Store) DeleteRespuesta(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM respuesta_rapida WHERE id=$1`, id)
	return err
}

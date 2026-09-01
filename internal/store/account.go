package store

import (
	"context"
	"strings"
	"time"
)

// CountRecentAccountCreations cuenta cuántas cuentas se crearon dentro de
// `window` para el mismo device_id O la misma IP. Sirve para aplicar el límite
// de 1 cuenta/mes por dispositivo y por IP. Ignora valores vacíos.
func (s *Store) CountRecentAccountCreations(ctx context.Context, deviceID, ip string, window time.Duration) (int, error) {
	deviceID = strings.TrimSpace(deviceID)
	ip = strings.TrimSpace(ip)
	if deviceID == "" && ip == "" {
		return 0, nil
	}
	since := time.Now().Add(-window)
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_creations
		WHERE created_at > $1
		  AND (
		        ($2 <> '' AND device_id = $2)
		     OR ($3 <> '' AND ip = $3)
		  )`, since, deviceID, ip).Scan(&count)
	return count, err
}

// RecordAccountCreation deja constancia de una cuenta creada (best-effort).
func (s *Store) RecordAccountCreation(ctx context.Context, deviceID, ip, country, email, app string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO account_creations (device_id, ip, country, email, app)
		VALUES (NULLIF($1,''), NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,''))`,
		strings.TrimSpace(deviceID), strings.TrimSpace(ip),
		strings.TrimSpace(country), strings.ToLower(strings.TrimSpace(email)), strings.TrimSpace(app))
	return err
}

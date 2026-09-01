package store

import (
	"context"
	"regexp"
	"strings"
)

// App es una aplicación del CRM (yape, interbank, …) para agrupar métricas.
type App struct {
	ID     string `json:"id"`
	Nombre string `json:"nombre"`
	Color  string `json:"color"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// SeedApps asegura que las apps base (yape, interbank, bcp) existan SIEMPRE,
// aunque la tabla ya tenga datos (así se añaden apps base nuevas en actualizaciones).
// No pisa el nombre/color si el usuario las editó.
func (s *Store) SeedApps(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO app (id, nombre, color) VALUES
		('yape','Yape','#34d399'),
		('interbank','Interbank','#38bdf8'),
		('bcp','BCP','#fbbf24')
		ON CONFLICT (id) DO NOTHING`)
	return err
}

func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, nombre, color FROM app ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []App{}
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Nombre, &a.Color); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateApp registra una nueva app (id derivado del nombre).
func (s *Store) CreateApp(ctx context.Context, nombre, color string) (App, error) {
	id := slug(nombre)
	if id == "" {
		id = "app"
	}
	if color == "" {
		color = "#a855f7"
	}
	a := App{ID: id, Nombre: strings.TrimSpace(nombre), Color: color}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO app (id, nombre, color) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET nombre=EXCLUDED.nombre, color=EXCLUDED.color`,
		a.ID, a.Nombre, a.Color)
	return a, err
}

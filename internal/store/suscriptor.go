package store

import (
	"context"
	"time"
)

// Suscriptor refleja la tabla suscriptor (antes modelo Prisma Suscriptor).
type Suscriptor struct {
	ID                   string
	Email                string
	Plan                 string
	Precio               int
	Meses                int
	FechaVencimiento     time.Time
	Activo               bool
	EnviarNotificaciones bool
	App                  string
}

// SuscriptoresParaNotificar replica prisma.suscriptor.findMany({ where: { enviarNotificaciones: true } }).
func (s *Store) SuscriptoresParaNotificar(ctx context.Context) ([]Suscriptor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, plan, precio, meses, fecha_vencimiento, activo, enviar_notificaciones, COALESCE(app,'')
		FROM suscriptor WHERE enviar_notificaciones = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suscriptor
	for rows.Next() {
		var x Suscriptor
		if err := rows.Scan(&x.ID, &x.Email, &x.Plan, &x.Precio, &x.Meses, &x.FechaVencimiento, &x.Activo, &x.EnviarNotificaciones, &x.App); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) SetSuscriptorActivo(ctx context.Context, email string, activo bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE suscriptor SET activo=$1 WHERE email=$2`, activo, email)
	return err
}

func (s *Store) SetSuscriptorEnviar(ctx context.Context, email string, enviar bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE suscriptor SET enviar_notificaciones=$1 WHERE email=$2`, enviar, email)
	return err
}

func (s *Store) DeleteSuscriptor(ctx context.Context, email string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM suscriptor WHERE email=$1`, email)
	return err
}

// RegistrarSuscripcion inserta/actualiza un suscriptor (lo usará la fase de pagos).
func (s *Store) RegistrarSuscripcion(ctx context.Context, x Suscriptor) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO suscriptor (id, email, plan, precio, meses, fecha_vencimiento, activo, enviar_notificaciones, app)
		VALUES ($1,$2,$3,$4,$5,$6,true,true,$7)
		ON CONFLICT (email) DO UPDATE SET
			plan=EXCLUDED.plan, precio=EXCLUDED.precio, meses=EXCLUDED.meses,
			fecha_vencimiento=EXCLUDED.fecha_vencimiento, activo=true, enviar_notificaciones=true, app=EXCLUDED.app`,
		x.ID, x.Email, x.Plan, x.Precio, x.Meses, x.FechaVencimiento, x.App)
	return err
}

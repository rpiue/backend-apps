package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Compra refleja la tabla compra (antes modelo Prisma Compra) — registro local
// del pago pendiente para recordatorios y conciliación.
type Compra struct {
	ID         string
	Email      string
	Codigo     string
	CompraID   string
	Estado     string
	App        string
	Monto      int
	Plan       string
	CreatedAt  time.Time
	Notificado bool
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CrearRegistroPago replica crearRegistroPago({...}): inserta una compra pendiente.
func (s *Store) CrearRegistroPago(ctx context.Context, email, codigo, compraID, app string, monto int, plan string) (Compra, error) {
	c := Compra{
		ID: newID(), Email: email, Codigo: codigo, CompraID: compraID,
		Estado: "pendiente", App: app, Monto: monto, Plan: plan, CreatedAt: time.Now(),
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO compra (id, email, codigo, compra_id, estado, app, monto, plan, created_at, notificado)
		VALUES ($1,$2,$3,$4,'pendiente',$5,$6,$7,$8,false)`,
		c.ID, c.Email, c.Codigo, c.CompraID, c.App, c.Monto, c.Plan, c.CreatedAt)
	return c, err
}

// EliminarRegistro replica eliminarRegistro(compraId): borra por compra_id.
func (s *Store) EliminarRegistro(ctx context.Context, compraID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM compra WHERE compra_id=$1`, compraID)
	return err
}

// PurgeComprasViejas borra compras pendientes más viejas que `antesDe`. Evita que
// códigos de pago vencidos (spam de /api/sara) llenen la tabla indefinidamente.
func (s *Store) PurgeComprasViejas(ctx context.Context, antesDe time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM compra WHERE estado='pendiente' AND created_at < $1`, antesDe)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ComprasPendientesNoNotificadas devuelve compras pendientes aún no notificadas
// (para el cron de recordatorios — reemplaza el node-schedule roto del JS).
func (s *Store) ComprasPendientesNoNotificadas(ctx context.Context, masNuevasQue time.Time) ([]Compra, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, codigo, compra_id, estado, COALESCE(app,''), COALESCE(monto,0), COALESCE(plan,''), created_at, notificado
		FROM compra WHERE estado='pendiente' AND notificado=false AND created_at >= $1`, masNuevasQue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Compra
	for rows.Next() {
		var c Compra
		if err := rows.Scan(&c.ID, &c.Email, &c.Codigo, &c.CompraID, &c.Estado, &c.App, &c.Monto, &c.Plan, &c.CreatedAt, &c.Notificado); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) MarcarNotificado(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE compra SET notificado=true WHERE id=$1`, id)
	return err
}

// RegistrarSuscripcionMeses replica registrarSuscripcion({...}): si existe, suma
// meses a la fecha de vencimiento actual; si no, crea desde hoy + meses.
func (s *Store) RegistrarSuscripcionMeses(ctx context.Context, email, plan string, precio, meses int, app string) error {
	var fecha time.Time
	err := s.pool.QueryRow(ctx, `SELECT fecha_vencimiento FROM suscriptor WHERE email=$1`, email).Scan(&fecha)
	if err != nil {
		// No existe → desde hoy.
		nueva := time.Now().AddDate(0, meses, 0)
		_, err = s.pool.Exec(ctx, `
			INSERT INTO suscriptor (id, email, plan, precio, meses, fecha_vencimiento, activo, enviar_notificaciones, app)
			VALUES ($1,$2,$3,$4,$5,$6,true,true,$7)`,
			newID(), email, plan, precio, meses, nueva, app)
		return err
	}
	// Existe → extender desde la fecha actual.
	nueva := fecha.AddDate(0, meses, 0)
	_, err = s.pool.Exec(ctx, `
		UPDATE suscriptor SET meses = meses + $1, precio=$2, plan=$3, app=$4,
			fecha_vencimiento=$5, activo=true, enviar_notificaciones=true WHERE email=$6`,
		meses, precio, plan, app, nueva, email)
	return err
}

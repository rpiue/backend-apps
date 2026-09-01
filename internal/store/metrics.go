package store

import (
	"context"
	"fmt"
	"time"
)

// Ingreso es un evento de ingreso: se registra CADA VEZ que se da acceso a un
// plan (negroide, webhook de pago, consumo de código). Alimenta la analítica.
type Ingreso struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Plan      string    `json:"plan"`
	Monto     int       `json:"monto"`
	App       string    `json:"app"`
	Fuente    string    `json:"fuente"` // "negroide" | "pago" | "codigo"
	CreatedAt time.Time `json:"createdAt"`
}

// RegistrarIngreso inserta un ingreso y lo devuelve.
func (s *Store) RegistrarIngreso(ctx context.Context, email, plan string, monto int, app, fuente string) (Ingreso, error) {
	ing := Ingreso{
		ID: newID(), Email: email, Plan: plan, Monto: monto,
		App: app, Fuente: fuente, CreatedAt: time.Now(),
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO metrica_ingreso (id, email, plan, monto, app, fuente, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ing.ID, ing.Email, ing.Plan, ing.Monto, ing.App, ing.Fuente, ing.CreatedAt)
	return ing, err
}

// RegistrarUsuarioMetrica anota la creación de un usuario (para la analítica de altas).
func (s *Store) RegistrarUsuarioMetrica(ctx context.Context, email, app string) {
	_, _ = s.pool.Exec(ctx, `INSERT INTO metrica_usuario (id, email, app) VALUES ($1,$2,$3)`, newID(), email, app)
}

type AppCount struct {
	App   string `json:"app"`
	Count int    `json:"count"`
}
type DiaCount struct {
	Fecha string `json:"fecha"`
	Count int    `json:"count"`
}

// UsuariosMetrics: altas de usuarios en un rango, total + desglose por app + serie diaria.
type UsuariosMetrics struct {
	Total  int        `json:"total"`
	PorApp []AppCount `json:"porApp"`
	Serie  []DiaCount `json:"serie"`
}

// GetUsuariosMetrics replica "cuántos usuarios se crearon entre fechas y en qué app".
func (s *Store) GetUsuariosMetrics(ctx context.Context, app, desde, hasta string) (UsuariosMetrics, error) {
	var m UsuariosMetrics
	if desde == "" {
		desde = "1970-01-01"
	}
	if hasta == "" {
		hasta = "2999-12-31"
	}
	hasApp := app != "" && app != "all"

	// Rangos half-open [desde, hasta+1día): NO castamos la columna created_at,
	// así el índice sobre created_at (y el compuesto lower(app),created_at) sí se usa.
	if hasApp {
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM metrica_usuario WHERE lower(app)=lower($1) AND created_at >= $2::date AND created_at < ($3::date + INTERVAL '1 day')`, app, desde, hasta).Scan(&m.Total)
	} else {
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM metrica_usuario WHERE created_at >= $1::date AND created_at < ($2::date + INTERVAL '1 day')`, desde, hasta).Scan(&m.Total)
	}

	// Por app (qué app creó más).
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(lower(app),'desconocida') a, count(*) c FROM metrica_usuario
		WHERE created_at >= $1::date AND created_at < ($2::date + INTERVAL '1 day') GROUP BY a ORDER BY c DESC`, desde, hasta)
	if err == nil {
		m.PorApp = []AppCount{}
		for rows.Next() {
			var x AppCount
			if rows.Scan(&x.App, &x.Count) == nil {
				m.PorApp = append(m.PorApp, x)
			}
		}
		rows.Close()
	}

	// Serie diaria.
	var rows2 interface {
		Next() bool
		Scan(...any) error
		Close()
	}
	if hasApp {
		rows2, err = s.pool.Query(ctx, `
			SELECT to_char(created_at,'YYYY-MM-DD') d, count(*) c FROM metrica_usuario
			WHERE lower(app)=lower($1) AND created_at >= $2::date AND created_at < ($3::date + INTERVAL '1 day')
			GROUP BY d ORDER BY d`, app, desde, hasta)
	} else {
		rows2, err = s.pool.Query(ctx, `
			SELECT to_char(created_at,'YYYY-MM-DD') d, count(*) c FROM metrica_usuario
			WHERE created_at >= $1::date AND created_at < ($2::date + INTERVAL '1 day') GROUP BY d ORDER BY d`, desde, hasta)
	}
	if err == nil {
		m.Serie = []DiaCount{}
		for rows2.Next() {
			var x DiaCount
			if rows2.Scan(&x.Fecha, &x.Count) == nil {
				m.Serie = append(m.Serie, x)
			}
		}
		rows2.Close()
	}
	return m, nil
}

type PlanRevenue struct {
	Plan  string `json:"plan"`
	Total int    `json:"total"`
	Count int    `json:"count"`
}
type SeriePunto struct {
	Fecha string `json:"fecha"`
	Total int    `json:"total"`
}

type Analytics struct {
	App              string        `json:"app"`
	IngresosTotal    int           `json:"ingresosTotal"`
	IngresosHoy      int           `json:"ingresosHoy"`
	IngresosMes      int           `json:"ingresosMes"`
	Suscriptores     int           `json:"suscriptores"`
	ComprasPend      int           `json:"comprasPendientes"`
	Conversaciones   int           `json:"conversaciones"`
	MensajesNoLeidos int           `json:"mensajesNoLeidos"`
	NecesitanAdmin   int           `json:"necesitanAdmin"`
	PorPlan          []PlanRevenue `json:"porPlan"`
	Serie30d         []SeriePunto  `json:"serie30d"`
	Serie12m         []SeriePunto  `json:"serie12m"`
}

// appFilter devuelve el fragmento WHERE/AND y el arg para filtrar por app.
// app vacío o "all" => sin filtro.
func appFilter(app string) (string, []any) {
	if app == "" || app == "all" {
		return "", nil
	}
	return "lower(app)=lower($1)", []any{app}
}

func (s *Store) GetAnalytics(ctx context.Context, app string) (Analytics, error) {
	a := Analytics{App: app}
	cond, args := appFilter(app)
	w := ""
	if cond != "" {
		w = " WHERE " + cond
	}
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso`+w, args...).Scan(&a.IngresosTotal)
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE created_at >= date_trunc('day', now()) AND created_at < date_trunc('day', now()) + INTERVAL '1 day'`+andCond(cond), args...).Scan(&a.IngresosHoy)
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE created_at >= date_trunc('month', now())`+andCond(cond), args...).Scan(&a.IngresosMes)

	// Estos no dependen de la app (globales).
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM suscriptor WHERE activo=true`).Scan(&a.Suscriptores)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM compra WHERE estado='pendiente'`).Scan(&a.ComprasPend)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM chat_conversacion`).Scan(&a.Conversaciones)
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(no_leidos),0) FROM chat_conversacion`).Scan(&a.MensajesNoLeidos)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM chat_conversacion WHERE necesita_admin=true`).Scan(&a.NecesitanAdmin)

	a.PorPlan = s.porPlan(ctx, cond, args)
	a.Serie30d = s.serie(ctx, "day", 30, cond, args)
	a.Serie12m = s.serie(ctx, "month", 12, cond, args)
	return a, nil
}

func andCond(cond string) string {
	if cond == "" {
		return ""
	}
	return " AND " + cond
}

func (s *Store) porPlan(ctx context.Context, cond string, args []any) []PlanRevenue {
	w := ""
	if cond != "" {
		w = " WHERE " + cond
	}
	rows, err := s.pool.Query(ctx, `
		SELECT plan, COALESCE(sum(monto),0) total, count(*) c
		FROM metrica_ingreso`+w+` GROUP BY plan ORDER BY total DESC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []PlanRevenue{}
	for rows.Next() {
		var p PlanRevenue
		if err := rows.Scan(&p.Plan, &p.Total, &p.Count); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) serie(ctx context.Context, unit string, n int, cond string, args []any) []SeriePunto {
	trunc, fmtStr, step := "day", "YYYY-MM-DD", "1 day"
	if unit == "month" {
		trunc, fmtStr, step = "month", "YYYY-MM", "1 month"
	}
	// Filtro de app calificado a la tabla m (cond = "lower(app)=lower($1)").
	join := ""
	if cond != "" {
		join = " AND lower(m.app)=lower($1)"
	}
	// Join por RANGO [d, d+step): no aplicamos date_trunc sobre m.created_at, así
	// el índice/BRIN sobre created_at sí se usa en cada franja de la serie.
	q := fmt.Sprintf(`
		WITH serie AS (
			SELECT generate_series(
				date_trunc('%[1]s', now()) - INTERVAL '%[2]d %[1]s',
				date_trunc('%[1]s', now()), INTERVAL '%[3]s') AS d
		)
		SELECT to_char(serie.d, '%[4]s') fecha, COALESCE(sum(m.monto),0) total
		FROM serie LEFT JOIN metrica_ingreso m
		  ON m.created_at >= serie.d AND m.created_at < serie.d + INTERVAL '%[3]s'%[5]s
		GROUP BY serie.d ORDER BY serie.d`, trunc, n, step, fmtStr, join)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []SeriePunto{}
	for rows.Next() {
		var p SeriePunto
		if err := rows.Scan(&p.Fecha, &p.Total); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// --- Vista de Ingresos (pagos por app/fecha) ---

type RevenueSummary struct {
	Hoy      int          `json:"hoy"`
	Semana   int          `json:"semana"`
	Mes      int          `json:"mes"`
	CountHoy int          `json:"countHoy"`
	Dias14   []SeriePunto `json:"dias14"`
	Pagos    []Ingreso    `json:"pagos"`
}

// Revenue arma el resumen de ingresos para una fecha (yyyy-mm-dd) y app.
func (s *Store) Revenue(ctx context.Context, app, fecha string) (RevenueSummary, error) {
	var rs RevenueSummary
	cond, args := appFilter(app)
	hasApp := cond != ""

	if hasApp {
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE lower(app)=lower($1) AND created_at >= $2::date AND created_at < $2::date + INTERVAL '1 day'`, app, fecha).Scan(&rs.Hoy)
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM metrica_ingreso WHERE lower(app)=lower($1) AND created_at >= $2::date AND created_at < $2::date + INTERVAL '1 day'`, app, fecha).Scan(&rs.CountHoy)
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE lower(app)=lower($1) AND created_at >= now() - INTERVAL '7 days'`, app).Scan(&rs.Semana)
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE lower(app)=lower($1) AND created_at >= now() - INTERVAL '30 days'`, app).Scan(&rs.Mes)
	} else {
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE created_at >= $1::date AND created_at < $1::date + INTERVAL '1 day'`, fecha).Scan(&rs.Hoy)
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM metrica_ingreso WHERE created_at >= $1::date AND created_at < $1::date + INTERVAL '1 day'`, fecha).Scan(&rs.CountHoy)
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE created_at >= now() - INTERVAL '7 days'`).Scan(&rs.Semana)
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(monto),0) FROM metrica_ingreso WHERE created_at >= now() - INTERVAL '30 days'`).Scan(&rs.Mes)
	}

	rs.Dias14 = s.serie(ctx, "day", 14, cond, args)

	rs.Pagos = []Ingreso{}
	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
	var err error
	if hasApp {
		rows, err = s.pool.Query(ctx, `
			SELECT id, email, plan, monto, COALESCE(app,''), COALESCE(fuente,''), created_at
			FROM metrica_ingreso WHERE lower(app)=lower($1) AND created_at >= $2::date AND created_at < $2::date + INTERVAL '1 day'
			ORDER BY created_at DESC LIMIT 200`, app, fecha)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, email, plan, monto, COALESCE(app,''), COALESCE(fuente,''), created_at
			FROM metrica_ingreso WHERE created_at >= $1::date AND created_at < $1::date + INTERVAL '1 day'
			ORDER BY created_at DESC LIMIT 200`, fecha)
	}
	if err != nil {
		return rs, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Ingreso
		if err := rows.Scan(&p.ID, &p.Email, &p.Plan, &p.Monto, &p.App, &p.Fuente, &p.CreatedAt); err == nil {
			rs.Pagos = append(rs.Pagos, p)
		}
	}
	return rs, nil
}

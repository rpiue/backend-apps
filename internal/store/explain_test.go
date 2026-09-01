package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestAnalyticsUsesIndex siembra datos y comprueba con EXPLAIN que las consultas
// de analítica (rango de fechas + app) usan índice y NO hacen Seq Scan sobre la
// tabla grande — el objetivo de la reescritura sargable.
func TestAnalyticsUsesIndex(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	ctx := context.Background()

	// BD de pruebas persistente: partimos de tabla limpia para que el seed sea idempotente.
	if _, err := s.pool.Exec(ctx, `TRUNCATE metrica_ingreso`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Sembrar ~60k ingresos repartidos en ~180 días y 3 apps.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO metrica_ingreso (id, email, plan, monto, app, fuente, created_at)
		SELECT md5(g::text), 'u'||g||'@x.com', 'Medium', 30,
		       (ARRAY['yape','bcp','interbank'])[1 + (g % 3)], 'pago',
		       now() - ((g % 180) || ' days')::interval
		FROM generate_series(1, 60000) g`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE metrica_ingreso`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// Consulta representativa: suma de una app en un rango de 7 días (patrón real).
	explain := func(q string, args ...any) string {
		rows, err := s.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			b.WriteString(line)
			b.WriteString("\n")
		}
		return b.String()
	}

	plan := explain(`SELECT COALESCE(sum(monto),0) FROM metrica_ingreso
		WHERE lower(app)=lower($1) AND created_at >= now() - INTERVAL '7 days'`, "bcp")
	t.Logf("PLAN app+rango:\n%s", plan)
	if strings.Contains(plan, "Seq Scan on metrica_ingreso") {
		t.Errorf("la consulta app+rango hace Seq Scan (índice no usado):\n%s", plan)
	}

	// Rango por día con el patrón half-open (antes usaba created_at::date = ...).
	plan2 := explain(`SELECT count(*) FROM metrica_ingreso
		WHERE created_at >= $1::date AND created_at < $1::date + INTERVAL '1 day'`,
		fmt.Sprintf("%d-01-15", 2025))
	t.Logf("PLAN día half-open:\n%s", plan2)
	if strings.Contains(plan2, "Seq Scan on metrica_ingreso") {
		t.Errorf("la consulta por día hace Seq Scan (índice no usado):\n%s", plan2)
	}
}

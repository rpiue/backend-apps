// Package store reemplaza Prisma + SQLite por Postgres (pgx).
// Modela las tablas Compra y Suscriptor del schema.prisma original, y deja
// preparada la base para el chat (estilo WhatsApp) de fases posteriores.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string, maxConns, minConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Dimensionado del pool para picos de escritura/lectura (~6k reg/día). Un pool
	// acotado evita saturar Postgres bajo ráfagas; MinConns mantiene conexiones
	// calientes; el reciclado evita conexiones zombis.
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	if minConns >= 0 {
		cfg.MinConns = int32(minConns)
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ctxPing, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctxPing); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() { s.pool.Close() }

// Migrate crea las tablas si no existen. Es idempotente y evita necesitar Prisma.
func (s *Store) Migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS compra (
    id          TEXT PRIMARY KEY,
    email       TEXT NOT NULL,
    codigo      TEXT,
    compra_id   TEXT NOT NULL,
    estado      TEXT NOT NULL DEFAULT 'pendiente',
    app         TEXT,
    monto       INTEGER,
    plan        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    notificado  BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS idx_compra_email ON compra(email);

CREATE TABLE IF NOT EXISTS suscriptor (
    id                     TEXT PRIMARY KEY,
    email                  TEXT NOT NULL,
    plan                   TEXT NOT NULL,
    precio                 INTEGER NOT NULL,
    meses                  INTEGER NOT NULL,
    fecha_vencimiento      TIMESTAMPTZ NOT NULL,
    activo                 BOOLEAN NOT NULL DEFAULT true,
    enviar_notificaciones  BOOLEAN NOT NULL DEFAULT true,
    app                    TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_suscriptor_email ON suscriptor(email);

-- Chat estilo WhatsApp (panel de admin <-> usuarios). Se llenará en fase de chat.
CREATE TABLE IF NOT EXISTS chat_conversacion (
    id             TEXT PRIMARY KEY,
    email          TEXT NOT NULL,
    app            TEXT NOT NULL DEFAULT 'yape',
    nombre         TEXT,
    ultimo_msg     TEXT,
    no_leidos      INTEGER NOT NULL DEFAULT 0,
    necesita_admin BOOLEAN NOT NULL DEFAULT false,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_conv_email_app ON chat_conversacion(email, app);
ALTER TABLE chat_conversacion ADD COLUMN IF NOT EXISTS necesita_admin BOOLEAN NOT NULL DEFAULT false;

-- Analítica: un evento por cada acceso/plan concedido (suma de ingresos).
CREATE TABLE IF NOT EXISTS metrica_ingreso (
    id          TEXT PRIMARY KEY,
    email       TEXT NOT NULL,
    plan        TEXT NOT NULL,
    monto       INTEGER NOT NULL DEFAULT 0,
    app         TEXT,
    fuente      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_metrica_created ON metrica_ingreso(created_at);
CREATE INDEX IF NOT EXISTS idx_metrica_plan ON metrica_ingreso(plan);
CREATE INDEX IF NOT EXISTS idx_metrica_app ON metrica_ingreso(lower(app));

-- Apps del CRM (yape, interbank, …) para agrupar métricas.
CREATE TABLE IF NOT EXISTS app (
    id          TEXT PRIMARY KEY,
    nombre      TEXT NOT NULL,
    color       TEXT NOT NULL DEFAULT '#a855f7',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Usuarios del panel admin (login email + contraseña).
CREATE TABLE IF NOT EXISTS admin_user (
    email         TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    nombre        TEXT,
    rol           TEXT NOT NULL DEFAULT 'admin',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Registro de creaciones de cuenta para limitar 1 cuenta/mes por dispositivo y
-- por IP, y para geolocalización. device_id = ANDROID_ID (sobrevive a reinstalar).
CREATE TABLE IF NOT EXISTS account_creations (
    id          BIGSERIAL PRIMARY KEY,
    device_id   TEXT,
    ip          TEXT,
    country     TEXT,
    email       TEXT,
    app         TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_acccre_device ON account_creations(device_id, created_at);
CREATE INDEX IF NOT EXISTS idx_acccre_ip ON account_creations(ip, created_at);

-- Respuestas rápidas configurables para el chat.
CREATE TABLE IF NOT EXISTS respuesta_rapida (
    id          TEXT PRIMARY KEY,
    texto       TEXT NOT NULL,
    orden       INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Métrica de usuarios creados (para "cuántos se crearon en tal día y en qué app").
CREATE TABLE IF NOT EXISTS metrica_usuario (
    id          TEXT PRIMARY KEY,
    email       TEXT NOT NULL,
    app         TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_metrica_usuario_created ON metrica_usuario(created_at);
CREATE INDEX IF NOT EXISTS idx_metrica_usuario_app ON metrica_usuario(lower(app));

CREATE TABLE IF NOT EXISTS chat_mensaje (
    id          BIGSERIAL PRIMARY KEY,
    conv_id     TEXT NOT NULL REFERENCES chat_conversacion(id) ON DELETE CASCADE,
    autor       TEXT NOT NULL,           -- 'admin' | 'user'
    cuerpo      TEXT NOT NULL,
    leido       BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_chat_msg_conv ON chat_mensaje(conv_id, created_at);

-- Códigos de activación cruzada (Yape↔BCP). El código de 6 chars NO revela la
-- app: la app y el estado (activo/usado) se resuelven aquí. Lookup O(1) por PK.
CREATE TABLE IF NOT EXISTS activation_code (
    code           TEXT PRIMARY KEY,
    app            TEXT NOT NULL,
    email          TEXT NOT NULL,
    plan           TEXT NOT NULL,
    estado         TEXT NOT NULL DEFAULT 'activo',   -- 'activo' | 'usado'
    used_at        TIMESTAMPTZ,
    used_by_email  TEXT,
    used_by_device TEXT,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Reutilizar un código vigente para el mismo (email, app, plan) sin escanear todo.
CREATE INDEX IF NOT EXISTS idx_actcode_lookup ON activation_code(email, app, plan) WHERE estado='activo';
-- Purga eficiente de expirados/usados.
CREATE INDEX IF NOT EXISTS idx_actcode_expires ON activation_code(expires_at);

-- ============================================================================
-- Índices de rendimiento (analítica a escala: ~6k registros/día).
-- Compuestos (app, fecha): cubren el patrón "ingresos/altas de una app en un
-- rango de fechas" con un solo índice, sin escanear fila por fila.
CREATE INDEX IF NOT EXISTS idx_metrica_ingreso_app_created ON metrica_ingreso(lower(app), created_at);
CREATE INDEX IF NOT EXISTS idx_metrica_usuario_app_created ON metrica_usuario(lower(app), created_at);
-- BRIN sobre created_at: diminuto y óptimo para rangos temporales en tablas
-- append-only muy grandes (los datos llegan ordenados por fecha).
CREATE INDEX IF NOT EXISTS idx_metrica_ingreso_created_brin ON metrica_ingreso USING brin (created_at);
CREATE INDEX IF NOT EXISTS idx_metrica_usuario_created_brin ON metrica_usuario USING brin (created_at);
CREATE INDEX IF NOT EXISTS idx_chat_mensaje_created_brin ON chat_mensaje USING brin (created_at);
-- Compras: el DELETE por compra_id y el cron de recordatorios/limpieza.
CREATE INDEX IF NOT EXISTS idx_compra_compra_id ON compra(compra_id);
CREATE INDEX IF NOT EXISTS idx_compra_pendientes ON compra(estado, notificado, created_at);
-- Suscriptores activos (conteo del dashboard).
CREATE INDEX IF NOT EXISTS idx_suscriptor_activo ON suscriptor(activo) WHERE activo=true;
`
	_, err := s.pool.Exec(ctx, schema)
	return err
}

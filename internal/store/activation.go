package store

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Alfabeto sin caracteres ambiguos (sin 0/O/1/I/L) para códigos legibles y que
// no se confunden al dictarlos. 32 símbolos → 32^6 ≈ 1.07e9 combinaciones, y el
// canje está protegido por rate limit + un solo uso.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// activationTTL: cuánto vive un código de activación (alineado con el mes del plan).
const activationTTL = 30 * 24 * time.Hour

// ErrCodeInvalido: el código no existe, ya se usó o expiró (mensaje genérico).
var ErrCodeInvalido = errors.New("código inválido o expirado")

func randCode(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(codeAlphabet)))
	for i := range b {
		idx, _ := rand.Int(rand.Reader, max)
		b[i] = codeAlphabet[idx.Int64()]
	}
	return string(b)
}

// GenerateActivationCode devuelve un código de 6 chars que activa `app` con `plan`
// para `email`. Es idempotente: si ya existe uno activo y vigente para el mismo
// (email, app, plan), lo reutiliza (no floodea la tabla si piden acceso repetido).
func (s *Store) GenerateActivationCode(ctx context.Context, email, app, plan string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	app = strings.ToLower(strings.TrimSpace(app))

	// 1) ¿Ya hay uno vigente? Reutilizarlo.
	var existing string
	err := s.pool.QueryRow(ctx, `
		SELECT code FROM activation_code
		WHERE email=$1 AND app=$2 AND plan=$3 AND estado='activo' AND expires_at > now()
		ORDER BY created_at DESC LIMIT 1`, email, app, plan).Scan(&existing)
	if err == nil && existing != "" {
		return existing, nil
	}
	if err != nil && err != pgx.ErrNoRows {
		return "", err
	}

	// 2) Crear uno nuevo, reintentando ante colisión de PK (muy improbable).
	expires := time.Now().Add(activationTTL)
	for attempt := 0; attempt < 6; attempt++ {
		code := randCode(6)
		_, err := s.pool.Exec(ctx, `
			INSERT INTO activation_code (code, app, email, plan, estado, expires_at)
			VALUES ($1,$2,$3,$4,'activo',$5)`, code, app, email, plan, expires)
		if err == nil {
			return code, nil
		}
		if !isUniqueViolation(err) {
			return "", err
		}
	}
	return "", errors.New("no se pudo generar un código único")
}

// RedeemActivationCode canjea un código de forma atómica y de un solo uso: marca
// 'usado' SOLO si estaba 'activo' y vigente, y devuelve la app y el plan a activar.
// Un segundo canje del mismo código no afecta filas → ErrCodeInvalido.
func (s *Store) RedeemActivationCode(ctx context.Context, code, email, device string) (app, plan string, err error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return "", "", ErrCodeInvalido
	}
	email = strings.ToLower(strings.TrimSpace(email))
	err = s.pool.QueryRow(ctx, `
		UPDATE activation_code
		SET estado='usado', used_at=now(), used_by_email=$2, used_by_device=$3
		WHERE code=$1 AND estado='activo' AND expires_at > now()
		RETURNING app, plan`, code, email, device).Scan(&app, &plan)
	if err == pgx.ErrNoRows {
		return "", "", ErrCodeInvalido
	}
	if err != nil {
		return "", "", err
	}
	return app, plan, nil
}

// RevertActivationCode repone un código a 'activo' (rollback best-effort) cuando
// el canje se marcó usado pero la activación posterior falló (p.ej. el usuario
// aún no había creado su cuenta en la app destino). Así puede reintentar.
func (s *Store) RevertActivationCode(ctx context.Context, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	_, err := s.pool.Exec(ctx, `
		UPDATE activation_code
		SET estado='activo', used_at=NULL, used_by_email=NULL, used_by_device=NULL
		WHERE code=$1 AND estado='usado'`, code)
	return err
}

// PurgeActivationCodes borra códigos expirados (unused) y usados antiguos, para
// que la tabla no crezca sin límite (anti-flood).
func (s *Store) PurgeActivationCodes(ctx context.Context, usadosAntesDe time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM activation_code
		WHERE (estado='activo' AND expires_at < now())
		   OR (estado='usado'  AND used_at < $1)`, usadosAntesDe)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// isUniqueViolation detecta el SQLSTATE 23505 (unique_violation) de Postgres.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

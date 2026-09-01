package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// testStore abre un Store contra un Postgres de pruebas (docker en 5442). Si no
// está disponible, se salta el test.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:test@localhost:5442/apiyc_test?sslmode=disable"
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 5, 1)
	if err != nil {
		t.Skipf("sin Postgres de pruebas (%s): %v", dsn, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestActivationCodeFlow(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	ctx := context.Background()
	email := "flow_" + time.Now().Format("150405.000000") + "@x.com"

	// 1) Generar es idempotente: dos veces → el mismo código.
	c1, err := s.GenerateActivationCode(ctx, email, "bcp", "Medium")
	if err != nil {
		t.Fatalf("generate 1: %v", err)
	}
	if len(c1) != 6 {
		t.Fatalf("el código debe tener 6 chars, fue %q", c1)
	}
	c2, err := s.GenerateActivationCode(ctx, email, "bcp", "Medium")
	if err != nil {
		t.Fatalf("generate 2: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("generate debería reutilizar el código vigente: %q vs %q", c1, c2)
	}

	// 2) El código no revela la app (solo alfabeto sin ambiguos).
	for _, r := range c1 {
		if r == '0' || r == 'O' || r == '1' || r == 'I' || r == 'L' {
			t.Fatalf("el código contiene un carácter ambiguo: %q", c1)
		}
	}

	// 3) Canje: resuelve app+plan y es de un solo uso.
	app, plan, err := s.RedeemActivationCode(ctx, c1, email, "dev-123")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if app != "bcp" || plan != "Medium" {
		t.Fatalf("canje resolvió app/plan incorrectos: %s/%s", app, plan)
	}
	if _, _, err := s.RedeemActivationCode(ctx, c1, email, "dev-123"); err != ErrCodeInvalido {
		t.Fatalf("segundo canje debería fallar con ErrCodeInvalido, fue %v", err)
	}

	// 4) Revert repone el código para reintentar.
	if err := s.RevertActivationCode(ctx, c1); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, _, err := s.RedeemActivationCode(ctx, c1, email, "dev-123"); err != nil {
		t.Fatalf("tras revert el canje debería funcionar: %v", err)
	}

	// 5) Código inexistente → genérico.
	if _, _, err := s.RedeemActivationCode(ctx, "ZZZZZZ", email, "dev"); err != ErrCodeInvalido {
		t.Fatalf("código inexistente debería dar ErrCodeInvalido, fue %v", err)
	}
}

func TestActivationCodeCaseInsensitive(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	ctx := context.Background()
	email := "case_" + time.Now().Format("150405.000000") + "@x.com"

	code, err := s.GenerateActivationCode(ctx, email, "yape", "Basico")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// El usuario lo escribe en minúsculas → debe canjear igual.
	app, _, err := s.RedeemActivationCode(ctx, toLowerASCII(code), email, "dev")
	if err != nil {
		t.Fatalf("canje case-insensitive: %v", err)
	}
	if app != "yape" {
		t.Fatalf("app esperada yape, fue %s", app)
	}
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

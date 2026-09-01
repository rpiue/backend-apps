package handlers

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex/backend/internal/config"
)

// TestPrivFromSeedB64 valida el parseo de la semilla y el rechazo de formatos
// inválidos (base64 corrupto o longitud incorrecta).
func TestPrivFromSeedB64(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	seedB64 := base64.StdEncoding.EncodeToString(priv.Seed())

	got, ok := privFromSeedB64(seedB64)
	if !ok {
		t.Fatalf("semilla válida rechazada")
	}
	if !got.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatalf("la clave derivada no corresponde a la pública original")
	}

	for _, bad := range []string{"", "no-base64!!", base64.StdEncoding.EncodeToString([]byte("corta"))} {
		if _, ok := privFromSeedB64(bad); ok {
			t.Fatalf("semilla inválida aceptada: %q", bad)
		}
	}
}

// TestPersistAndReloadSeed comprueba el round-trip: al persistir una semilla y
// releerla se obtiene la MISMA clave pública (estabilidad entre reinicios).
func TestPersistAndReloadSeed(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{FirebaseCredsDir: dir}
	path := accessSeedPath(cfg)
	if path != filepath.Join(dir, accessSeedFile) {
		t.Fatalf("ruta inesperada: %s", path)
	}

	_, priv, _ := ed25519.GenerateKey(nil)
	seedB64 := base64.StdEncoding.EncodeToString(priv.Seed())
	if err := persistAccessSeed(path, seedB64); err != nil {
		t.Fatalf("persistAccessSeed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	reloaded, ok := privFromSeedB64(strings.TrimSpace(string(raw)))
	if !ok {
		t.Fatalf("no se pudo releer la semilla persistida")
	}
	if !reloaded.Public().(ed25519.PublicKey).Equal(priv.Public().(ed25519.PublicKey)) {
		t.Fatalf("la clave releída difiere de la persistida")
	}
}

// TestAccessSeedPathDefault verifica el default ./secrets cuando no hay config.
func TestAccessSeedPathDefault(t *testing.T) {
	if got := accessSeedPath(nil); got != filepath.Join("./secrets", accessSeedFile) {
		t.Fatalf("default inesperado: %s", got)
	}
}

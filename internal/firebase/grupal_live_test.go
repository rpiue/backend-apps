package firebase

import (
	"context"
	"os"
	"testing"
)

// TestLiveGrupalGrant reproduce el flujo del panel "Dar plan grupal" contra el
// Firebase REAL: GenerarCodigosParaUsuario(opcion) para un plan grupal. Solo corre
// con FB_LIVE_TEST=1 (usa las apikeys por defecto del registry). Sirve para ver el
// error exacto que el usuario reporta al dar un plan grupal.
//
//	Basico Grupal -> opcion 1 | Medium Grupal -> 2 | Medium Grupal Plus -> 3
func TestLiveGrupalGrant(t *testing.T) {
	if os.Getenv("FB_LIVE_TEST") != "1" {
		t.Skip("set FB_LIVE_TEST=1 para correr contra el Firebase real")
	}
	c := New()
	userDB, _ := c.Registry.UserDB("yape")
	email := os.Getenv("FB_TEST_EMAIL")
	if email == "" {
		email = "malcapedro00@gmail.com"
	}
	res, err := c.GenerarCodigosParaUsuario(context.Background(), userDB, email, "Pedro Test", 1)
	if err != nil {
		t.Fatalf("GenerarCodigosParaUsuario ERROR: %v", err)
	}
	t.Logf("OK success=%v msg=%q plan=%s codigos=%d creados=%d fecha=%s benef=%v",
		res.Success, res.Message, res.Plan, len(res.Codigos), res.Creados, res.FechaFinal, res.Beneficiarios)
}

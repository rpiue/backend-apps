package mercadopago

import (
	"context"
	"os"
	"testing"
)

// TestLiveCrearPagoEfectivo ejerce el código Go REAL contra la API de MercadoPago.
// Solo corre con MP_LIVE_TEST=1 y MP_TOKEN=<token> (no en la suite normal, para no
// depender de red ni crear pagos). Verifica que el parseo de verification_code /
// external_resource_url funciona con la respuesta real.
func TestLiveCrearPagoEfectivo(t *testing.T) {
	if os.Getenv("MP_LIVE_TEST") != "1" {
		t.Skip("set MP_LIVE_TEST=1 y MP_TOKEN para correr el test en vivo")
	}
	c := New(os.Getenv("MP_TOKEN"))
	res, err := c.CrearPagoEfectivo(context.Background(), PagoEfectivoInput{
		Email: "malcapedro00@gmail.com", Nombre: "Pedro Test", Monto: 30,
		Descripcion: "Basico", Plan: "Basico", App: "yape", PlanN: 0,
		CompraID: "gotest-" + os.Getenv("MP_IDEM"), NotifyURL: "https://codexpe.com/api/webhook",
	})
	if err != nil {
		t.Fatalf("CrearPagoEfectivo error: %v", err)
	}
	if res.Codigo == "" {
		t.Fatalf("sin código en la respuesta: %+v", res)
	}
	t.Logf("OK  status=%s  id=%s  codigo=%s  ticket=%s", res.Status, res.ID, res.Codigo, res.Ticket)
}

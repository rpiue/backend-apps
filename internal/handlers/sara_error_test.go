package handlers

import "testing"

// La respuesta de error de /sara debe replicar index.js: el `message` es el
// detallado que devuelve generarLinkPago, no uno genérico; y `error` es el objeto
// original o null. El frontend depende de ese texto (era la causa del "error").
func TestSaraErrorBody(t *testing.T) {
	// Caso scrape fallido: generarLinkPago devolvió un mensaje detallado.
	detalle := "No pudimos leer el código automáticamente.\nRevisa tu correo: a@b.com Mercado Pago envió tu código de pago."
	res := map[string]any{"status": false, "message": detalle}
	body := saraErrorBody(res)

	if body["success"] != false {
		t.Fatalf("success debía ser false, got %v", body["success"])
	}
	if body["message"] != detalle {
		t.Fatalf("message debía ser el detallado del JS, got %q", body["message"])
	}
	if body["error"] == nil {
		t.Fatal("error debía llevar el objeto original (no nil)")
	}

	// Caso sin respuesta (res nil): mensaje genérico y error null, como `paymentLink || null`.
	body = saraErrorBody(nil)
	if body["message"] != "No se pudo generar el link de pago" {
		t.Fatalf("sin res, message debía ser el genérico, got %q", body["message"])
	}
	if body["error"] != nil {
		t.Fatalf("sin res, error debía ser null, got %v", body["error"])
	}
}

package chat

import "strings"

import "testing"

// El mensaje del código cruzado debe llevar el formato que pidió el negocio:
// nombre comercial de la app (sin "Fake"), la web de descarga y el código.
func TestMensajeCodigoCruzado(t *testing.T) {
	msg := mensajeCodigoCruzado("codexpe.com", CrossGrant{App: "bcp", Codigo: "AB3K9P", Plan: "Medium"})
	for _, sub := range []string{"BCP", "codexpe.com", "AB3K9P", "Medium"} {
		if !strings.Contains(msg, sub) {
			t.Fatalf("el mensaje no contiene %q:\n%s", sub, msg)
		}
	}
	// No debe filtrar el nombre interno "Fake" al cliente.
	if strings.Contains(msg, "Fake") {
		t.Fatalf("el mensaje al cliente no debe contener 'Fake':\n%s", msg)
	}
}

// appNombreCliente traduce los nombres internos a los comerciales.
func TestAppNombreCliente(t *testing.T) {
	casos := map[string]string{"bcp": "BCP", "yape": "Yape", "interbank": "Interbank", "": "Yape"}
	for in, want := range casos {
		if got := appNombreCliente(in); got != want {
			t.Fatalf("appNombreCliente(%q) = %q; want %q", in, got, want)
		}
	}
}

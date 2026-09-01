package firebase

import (
	"strings"
	"testing"
)

// La API :commit de Firestore exige el nombre RELATIVO del recurso (empieza en
// "projects/…"). Con la URL completa respondía 400 "lacks 'projects' at index 0"
// y las escrituras por lote (códigos grupales, consumirCodigo) fallaban.
func TestProjectFullName(t *testing.T) {
	p := Project{ID: "controlpagos-1262b"}
	got := p.fullName("codigosApp/malcapedro00@gmail.com/codigos/abc123")
	want := "projects/controlpagos-1262b/databases/(default)/documents/codigosApp/malcapedro00@gmail.com/codigos/abc123"
	if got != want {
		t.Fatalf("fullName = %q; want %q", got, want)
	}
	if strings.HasPrefix(got, "http") {
		t.Fatalf("el name NO debe llevar el prefijo host/versión; commit lo rechaza: %q", got)
	}
	if !strings.HasPrefix(got, "projects/") {
		t.Fatalf("el name debe empezar en 'projects/': %q", got)
	}
}

func TestProjectTxEndpoint(t *testing.T) {
	p := Project{ID: "controlpagos-1262b", APIKey: "test-key"}

	if got, want := p.txEndpoint("beginTransaction"), "https://firestore.googleapis.com/v1/projects/controlpagos-1262b/databases/(default)/documents:beginTransaction?key=test-key"; got != want {
		t.Fatalf("txEndpoint(beginTransaction) = %q; want %q", got, want)
	}
	if got, want := p.txEndpoint("rollback"), "https://firestore.googleapis.com/v1/projects/controlpagos-1262b/databases/(default)/documents:rollback?key=test-key"; got != want {
		t.Fatalf("txEndpoint(rollback) = %q; want %q", got, want)
	}
}

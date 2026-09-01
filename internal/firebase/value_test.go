package firebase

import (
	"testing"
	"time"
)

// Un timestamp LEÍDO de Firestore (tsJSON) debe poder REESCRIBIRSE tal cual.
// Antes caía en el `default` de encodeValue y se guardaba como nullValue: al
// renovar los códigos grupales, el espejo `createdAt` de codigosIndex se
// borraba (quedaba en null) en cada llamada.
func TestEncodeValueTimestampLeido(t *testing.T) {
	orig := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	doc := decodeFields(map[string]any{
		"createdAt": map[string]any{"timestampValue": orig.Format(time.RFC3339Nano)},
	})

	got := encodeValue(doc["createdAt"])
	ts, ok := got["timestampValue"].(string)
	if !ok {
		t.Fatalf("createdAt releído se codificó como %v; se esperaba timestampValue", got)
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("timestampValue no parseable (%q): %v", ts, err)
	}
	if !parsed.Equal(orig) {
		t.Fatalf("round-trip cambió la fecha: %s != %s", parsed, orig)
	}
}

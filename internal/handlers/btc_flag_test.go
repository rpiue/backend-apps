package handlers

import "testing"

// btcFlagEfectivo combina el soporte del servidor con el flag por-app. Las reglas
// importan: sin servidor NUNCA hay BTC; con servidor y flag ausente, por defecto
// SÍ (no regresar apps existentes); el flag solo puede desactivarlo explícito.
func TestBtcFlagEfectivo(t *testing.T) {
	casos := []struct {
		nombre    string
		serverBtc bool
		datos     map[string]any
		want      bool
	}{
		{"sin servidor, sin doc", false, nil, false},
		{"sin servidor, flag true", false, map[string]any{"btcDisponible": true}, false},
		{"servidor, sin doc", true, nil, true},
		{"servidor, doc sin flag", true, map[string]any{"version": "1.0"}, true},
		{"servidor, flag true", true, map[string]any{"btcDisponible": true}, true},
		{"servidor, flag false", true, map[string]any{"btcDisponible": false}, false},
		{"servidor, flag string 'false'", true, map[string]any{"btcDisponible": "false"}, false},
		{"servidor, flag string 'true'", true, map[string]any{"btcDisponible": "true"}, true},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			if got := btcFlagEfectivo(c.serverBtc, c.datos); got != c.want {
				t.Fatalf("btcFlagEfectivo(%v, %v) = %v; want %v", c.serverBtc, c.datos, got, c.want)
			}
		})
	}
}

func TestFirebaseBool(t *testing.T) {
	casos := []struct {
		in   any
		want bool
	}{
		{true, true}, {false, false},
		{"true", true}, {"TRUE", true}, {"1", true}, {"si", true}, {"sí", true},
		{"false", false}, {"0", false}, {"", false}, {"nope", false},
		{nil, false}, {42, false},
	}
	for _, c := range casos {
		if got := firebaseBool(c.in); got != c.want {
			t.Fatalf("firebaseBool(%#v) = %v; want %v", c.in, got, c.want)
		}
	}
}

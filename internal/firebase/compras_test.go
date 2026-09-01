package firebase

import "testing"

// Firestore (vía REST) puede devolver el contador como int64, o como float64 si
// alguna vez se guardó como double, o como string. comprasComoInt debe normalizar
// todos esos casos; si no, el "+1" partiría de 0 y se perdería el histórico.
func TestComprasComoInt(t *testing.T) {
	casos := []struct {
		nombre string
		in     any
		want   int64
	}{
		{"ausente", nil, 0},
		{"int64", int64(4), 4},
		{"int", 7, 7},
		{"float64 entero", float64(3), 3},
		{"string numérico", "12", 12},
		{"string con espacios", "  5 ", 5},
		{"string basura", "abc", 0},
		{"tipo inesperado", []int{1}, 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			if got := comprasComoInt(c.in); got != c.want {
				t.Fatalf("comprasComoInt(%v) = %d; want %d", c.in, got, c.want)
			}
		})
	}
}

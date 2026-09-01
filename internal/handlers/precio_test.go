package handlers

import "testing"

// Catálogo con la MISMA forma que devuelve Firebase en prod (yape): `titulo` +
// `precio` como string. Reproduce el caso que reportó el usuario: Medium Grupal
// Plus cuesta 90 en la BD, no 130.
func catalogoProd() (planes, planGrupal []map[string]any) {
	planes = []map[string]any{
		{"titulo": "Plan Basico", "precio": "30", "n": int64(1)},
		{"titulo": "Plan Medium", "precio": "35", "n": int64(2)},
	}
	planGrupal = []map[string]any{
		{"titulo": "Basico Grupal", "precio": "45"},
		{"titulo": "Medium Grupal", "precio": "65"},
		{"titulo": "Medium Grupal Plus", "precio": "90"},
	}
	return
}

func TestPrecioDeCatalogo(t *testing.T) {
	planes, grupal := catalogoProd()
	casos := map[string]int{
		"Basico":             30,
		"Medium":             35,
		"Basico Grupal":      45,
		"Medium Grupal":      65,
		"Medium Grupal Plus": 90, // el del reporte: 90, NO 130
	}
	for plan, want := range casos {
		t.Run(plan, func(t *testing.T) {
			got, ok := precioDeCatalogo(planes, grupal, plan)
			if !ok {
				t.Fatalf("no se encontró precio para %q", plan)
			}
			if got != want {
				t.Fatalf("precio de %q = %d; se esperaba %d (el de la BD)", plan, got, want)
			}
		})
	}
}

// Un plan que no está en el catálogo no debe devolver precio (cae al fallback).
func TestPrecioDeCatalogo_NoExiste(t *testing.T) {
	planes, grupal := catalogoProd()
	if _, ok := precioDeCatalogo(planes, grupal, "Premium"); ok {
		t.Fatal("Premium no está en el catálogo; no debía encontrar precio")
	}
}

func TestParsePrecio(t *testing.T) {
	casos := []struct {
		in   any
		want int
		ok   bool
	}{
		{"90", 90, true}, {"45", 45, true}, {" 30 ", 30, true},
		{"S/ 90", 90, true}, {"90.00", 90, true},
		{int64(65), 65, true}, {float64(35), 35, true}, {35, 35, true},
		{"0", 0, false}, {"", 0, false}, {"gratis", 0, false}, {nil, 0, false},
	}
	for _, c := range casos {
		got, ok := parsePrecio(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("parsePrecio(%#v) = (%d,%v); se esperaba (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

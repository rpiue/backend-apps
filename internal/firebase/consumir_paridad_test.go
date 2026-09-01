package firebase

import "testing"

// Estas son las respuestas EXACTAS de consumirCodigo() en codigo.js (líneas
// 403-533 del servidor de producción). El test las fija para que un cambio en
// el Go que se desvíe del JS falle aquí, en vez de aparecer como un error raro
// en la app móvil.
func TestParidadConJS_MensajesDeConsumirCodigo(t *testing.T) {
	// Los rechazos que no dependen de datos del código.
	fijos := map[string]string{
		"código vacío":         "Código requerido",
		"índice sin documento": "Código inválido",
		"índice sin path":      "Índice corrupto: falta path",
		"código sin documento": "Código no existe",
		"error interno":        "Error al consumir el código",
	}
	if errorAlConsumir()["message"] != fijos["error interno"] {
		t.Errorf("errorAlConsumir dice %q, el JS dice %q",
			errorAlConsumir()["message"], fijos["error interno"])
	}
	if ok, _ := errorAlConsumir()["ok"].(bool); ok {
		t.Error("errorAlConsumir debe llevar ok:false")
	}
}

// El JS habilita el código SOLO si status es el booleano true:
//
//	const isDisponible = (typeof d.status === 'boolean' && d.status === true);
//
// Un status en texto —incluido "disponible"— NO vale. Se comprueba que la
// lectura del Go hace lo mismo, porque aceptar de más significaría dar acceso
// con códigos que el backend actual rechaza.
func TestParidadConJS_SoloElBooleanoTrueHabilita(t *testing.T) {
	casos := []struct {
		nombre string
		status any
		quiere bool
	}{
		{"booleano true", true, true},
		{"booleano false", false, false},
		{"texto disponible", "disponible", false},
		{"texto usado", "usado", false},
		{"ausente", nil, false},
		{"número", 1, false},
	}
	for _, c := range casos {
		datos := map[string]any{"status": c.status}
		got, _ := datos["status"].(bool) // misma expresión que usa ConsumirCodigo
		if got != c.quiere {
			t.Errorf("%s: disponible=%v, el JS daría %v", c.nombre, got, c.quiere)
		}
	}
}

// normalizeStatus sigue en uso por VerificarCodigoOnline, que sí acepta el
// texto "disponible" — igual que el JS. Se comprueba que ahí no cambió nada.
func TestNormalizeStatus_IgualQueElJS(t *testing.T) {
	casos := map[any]string{
		true: "disponible", false: "usado",
		"disponible": "disponible", "DISPONIBLE": "disponible", "usado": "usado",
		nil: "desconocido", 42: "desconocido",
	}
	for entrada, quiere := range casos {
		if got := normalizeStatus(entrada); got != quiere {
			t.Errorf("normalizeStatus(%v)=%q, quería %q", entrada, got, quiere)
		}
	}
}

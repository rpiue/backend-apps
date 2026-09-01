package handlers

import (
	"strconv"
	"strings"
	"time"
)

// nowDate devuelve la fecha de hoy (YYYY-MM-DD) en hora Lima.
func nowDate() string {
	loc, err := time.LoadLocation("America/Lima")
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc).Format("2006-01-02")
}

// toStr convierte un valor JSON (string/número/bool/nil) a string, como hacía JS.
func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

// toInt convierte un valor JSON a int (los metadata de MP pueden venir como string o número).
func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	case int:
		return x
	}
	return 0
}

// parsePrecio interpreta el precio de un plan tal como lo guarda Firebase: casi
// siempre un STRING ("90", a veces "S/ 90" o "90.00"), pero también puede venir
// como número. Devuelve (precio, true) solo si es un entero positivo.
func parsePrecio(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), x > 0
	case int:
		return x, x > 0
	case float64:
		return int(x), x > 0
	case string:
		s := strings.TrimSpace(x)
		s = strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(s), "S/"))
		s = strings.TrimSpace(strings.TrimPrefix(s, "."))
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && f > 0 {
			return int(f), true
		}
	}
	return 0, false
}

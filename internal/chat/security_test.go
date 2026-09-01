package chat

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"codex/backend/internal/middleware"
)

// La huella (user-agent + IP) es lo que ata una sesión del chat y un ticket de
// WebSocket a quien los pidió. Si la IP se puede falsificar con una cabecera,
// esa atadura no protege de nada: un ticket robado vale desde cualquier sitio.
func TestFingerprint_NoSeCreeLaIPDeUnDesconocido(t *testing.T) {
	// Limitador que solo confía en 10.0.0.0/8 (la red del proxy).
	lim := middleware.NewLimiter(nil, []string{"10.0.0.0/8"})
	s := &Service{lim: lim}

	// Petición directa desde una IP cualquiera de internet, mintiendo sobre
	// quién es para hacerse pasar por la víctima.
	atacante := httptest.NewRequest(http.MethodGet, "/api/chat/ws", nil)
	atacante.RemoteAddr = "203.0.113.9:44321"
	atacante.Header.Set("X-Forwarded-For", "198.51.100.7") // IP de la víctima
	atacante.Header.Set("cf-connecting-ip", "198.51.100.7")
	atacante.Header.Set("User-Agent", "curl/8")

	// La misma víctima, llegando de verdad a través del proxy de confianza.
	victima := httptest.NewRequest(http.MethodGet, "/api/chat/ws", nil)
	victima.RemoteAddr = "10.0.0.5:5000" // Caddy
	victima.Header.Set("X-Forwarded-For", "198.51.100.7")
	victima.Header.Set("User-Agent", "curl/8")

	fpAtacante := s.fingerprintFromRequest(atacante)
	fpVictima := s.fingerprintFromRequest(victima)

	if fpAtacante.ipHash == fpVictima.ipHash {
		t.Fatal("la huella del atacante coincide con la de la víctima: la cabecera de IP se está creyendo sin comprobar el proxy")
	}
	if s.clientIP(atacante) != "203.0.113.9" {
		t.Fatalf("de un peer no confiable debe usarse la IP del socket, se obtuvo %q", s.clientIP(atacante))
	}
	if s.clientIP(victima) != "198.51.100.7" {
		t.Fatalf("del proxy de confianza sí debe leerse la cabecera, se obtuvo %q", s.clientIP(victima))
	}
}

// Sin limitador configurado tampoco debe caerse en las cabeceras: mejor una IP
// menos precisa que una IP que elige quien ataca.
func TestClientIP_SinLimitadorIgnoraCabeceras(t *testing.T) {
	s := &Service{}
	r := httptest.NewRequest(http.MethodGet, "/api/chat/ws", nil)
	r.RemoteAddr = "203.0.113.9:44321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("se obtuvo %q; debe usarse la IP del socket, nunca la cabecera", got)
	}
}

// El Content-Type de los adjuntos antiguos viene de la base de datos, donde lo
// escribió el sistema anterior sin comprobarlo. No debe salir tal cual.
func TestTipoSeguro(t *testing.T) {
	casos := map[string]string{
		// Tipos legítimos: pasan intactos.
		"image/jpeg":               "image/jpeg",
		"video/mp4":                "video/mp4",
		"audio/mpeg":               "audio/mpeg",
		"IMAGE/PNG":                "image/png",  // se normaliza a minúsculas
		"image/gif; charset=utf-8": "image/gif",  // se descartan los parámetros
		"  video/webm  ":           "video/webm", // espacios sobrantes

		// Lo que un adjunto antiguo podría llevar registrado y NO debe servirse
		// como tal, porque el navegador lo interpretaría.
		"text/html":                "application/octet-stream",
		"image/svg+xml":            "application/octet-stream",
		"application/javascript":   "application/octet-stream",
		"application/x-httpd-php":  "application/octet-stream",
		"application/octet-stream": "application/octet-stream",
		"":                         "application/octet-stream",
	}
	for entrada, esperado := range casos {
		if got := tipoSeguro(entrada); got != esperado {
			t.Errorf("tipoSeguro(%q)=%q, se esperaba %q", entrada, got, esperado)
		}
	}
}

// Un Content-Type con salto de línea no debe poder inyectar cabeceras.
func TestTipoSeguro_NoPermiteInyeccion(t *testing.T) {
	sucio := "image/png\r\nSet-Cookie: robado=1"
	if got := tipoSeguro(sucio); got != "application/octet-stream" {
		t.Fatalf("se obtuvo %q; un tipo con saltos de línea debe caer al genérico", got)
	}
}

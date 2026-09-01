package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// proxyDePrueba implementa TrustChecker devolviendo siempre lo que se le diga.
type proxyDePrueba bool

func (p proxyDePrueba) IsTrustedPeer(*http.Request) bool { return bool(p) }

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// El caso que importa: un POST en claro NO se atiende. Si esto se rompiera, las
// contraseñas volverían a viajar interceptables sin que nadie lo note.
func TestForceHTTPS_RechazaPOSTEnClaro(t *testing.T) {
	h := ForceHTTPS(true, proxyDePrueba(false))(http.HandlerFunc(ok))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://api.local/api/login", strings.NewReader(`{}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("un POST sin TLS debe dar 403, dio %d", rec.Code)
	}
}

func TestForceHTTPS_RedirigeGET(t *testing.T) {
	h := ForceHTTPS(true, proxyDePrueba(false))(http.HandlerFunc(ok))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://api.local/api/algo", nil))
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("un GET sin TLS debe redirigir con 308, dio %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://") {
		t.Fatalf("la redirección debe apuntar a https, apunta a %q", loc)
	}
}

// El núcleo de la defensa: X-Forwarded-Proto solo vale si lo manda un proxy
// confiable. Si no, cualquiera se declararía "cifrado" y el bloqueo sería humo.
func TestForceHTTPS_NoSeCreeLaCabeceraDeUnDesconocido(t *testing.T) {
	h := ForceHTTPS(true, proxyDePrueba(false))(http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodPost, "http://api.local/api/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https") // el atacante miente
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no se debe confiar en X-Forwarded-Proto de un peer no confiable; dio %d", rec.Code)
	}
}

func TestForceHTTPS_AceptaLaCabeceraDelProxyConfiable(t *testing.T) {
	h := ForceHTTPS(true, proxyDePrueba(true))(http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodPost, "http://api.local/api/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("tras un proxy confiable que terminó TLS debe pasar; dio %d", rec.Code)
	}
}

func TestForceHTTPS_HealthCheckPasaSinTLS(t *testing.T) {
	h := ForceHTTPS(true, proxyDePrueba(false))(http.HandlerFunc(ok))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://api.local/codex", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("el health check debe responder sin TLS; dio %d", rec.Code)
	}
}

// HSTS por HTTP es inútil (el navegador lo ignora) y en desarrollo dejaría
// localhost clavado en "solo https". Solo debe salir cuando hubo TLS.
func TestSecurityHeaders_HSTSSoloConTLS(t *testing.T) {
	sinTLS := httptest.NewRecorder()
	SecurityHeaders(true, proxyDePrueba(false), nil)(http.HandlerFunc(ok)).
		ServeHTTP(sinTLS, httptest.NewRequest(http.MethodGet, "http://api.local/api/x", nil))
	if sinTLS.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("no debe mandarse HSTS por HTTP en claro")
	}

	conTLS := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://api.local/api/x", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	SecurityHeaders(true, proxyDePrueba(true), nil)(http.HandlerFunc(ok)).ServeHTTP(conTLS, r)
	if !strings.Contains(conTLS.Header().Get("Strict-Transport-Security"), "max-age=63072000") {
		t.Fatalf("falta HSTS sobre TLS: %q", conTLS.Header().Get("Strict-Transport-Security"))
	}
	if conTLS.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("falta X-Content-Type-Options")
	}
}

func TestSecretoIgual(t *testing.T) {
	casos := []struct {
		a, b string
		want bool
	}{
		{"clave-larga-123", "clave-larga-123", true},
		{"clave-larga-123", "clave-larga-124", false},
		{"", "", false},     // vacío nunca autentica
		{"", "algo", false}, // una API key sin configurar no debe abrir la puerta
		{"algo", "", false},
	}
	for _, c := range casos {
		if got := SecretoIgual(c.a, c.b); got != c.want {
			t.Errorf("SecretoIgual(%q,%q)=%v, quería %v", c.a, c.b, got, c.want)
		}
	}
}

func TestOriginChecker(t *testing.T) {
	check := OriginChecker([]string{"https://codexpe.com", "http://localhost:5173"})
	casos := map[string]bool{
		"":                      true, // cliente nativo (app móvil)
		"https://codexpe.com":   true,
		"https://codexpe.com/":  true, // barra final tolerada
		"http://localhost:5173": true,
		"https://malicioso.com": false,
		"null":                  false,
	}
	for origin, want := range casos {
		r := httptest.NewRequest(http.MethodGet, "http://api.local/ws", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if got := check(r); got != want {
			t.Errorf("Origin %q: got %v, quería %v", origin, got, want)
		}
	}
}

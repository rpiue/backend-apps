package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// TrustChecker sabe si una petición llegó a través de un proxy de confianza.
// Lo cumple *Limiter; se declara como interfaz para no acoplar los middlewares
// de seguridad al rate limiter.
type TrustChecker interface {
	IsTrustedPeer(r *http.Request) bool
}

// EsHTTPS decide si la petición viajó cifrada de extremo a extremo.
//
// Dos formas de saberlo:
//   - r.TLS != nil  → el propio Go terminó el TLS: certeza total.
//   - X-Forwarded-Proto: https → lo dice el proxy que terminó el TLS.
//
// La cabecera SOLO se cree si el peer inmediato está en TRUSTED_PROXIES. Si no,
// cualquier cliente podría mandar "X-Forwarded-Proto: https" desde una conexión
// en claro y saltarse el bloqueo: la cabecera de un desconocido no es prueba de
// nada. Esa distinción es justamente lo que hace útil al middleware.
func EsHTTPS(r *http.Request, trust TrustChecker) bool {
	if r.TLS != nil {
		return true
	}
	if trust == nil || !trust.IsTrustedPeer(r) {
		return false
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = r.Header.Get("X-Forwarded-Protocol")
	}
	if strings.EqualFold(strings.TrimSpace(proto), "https") {
		return true
	}
	// Cloudflare y algunos balanceadores usan estas en su lugar.
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Ssl")), "on") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Cf-Visitor")), `{"scheme":"https"}`)
}

// ForceHTTPS es el parche de aplicación contra man-in-the-middle.
//
// Sin TLS, todo lo que cruza la red va en claro: el JWT del panel, la contraseña
// del login, el ticket del WebSocket y los mensajes del chat. Cualquiera en la
// ruta (el wifi del local, el ISP, un router comprometido, otro inquilino del
// datacenter con ARP spoofing) puede leerlos y —peor— MODIFICARLOS al vuelo:
// cambiar el monto de un pago, inyectar respuestas o robar la sesión de admin.
//
// Con este middleware activo el servidor simplemente NO atiende peticiones en
// claro: a GET/HEAD les responde un 308 hacia la misma URL en https (para que el
// navegador reintente cifrado), y a los métodos con cuerpo —POST, PUT, DELETE—
// les responde 403 sin procesarlos. Un redirect no sirve para un POST: el cuerpo
// (contraseña, token, mensaje) YA viajó en claro y ya fue interceptado; lo único
// correcto es rechazarlo para que quien lo mandó corrija el destino.
//
// Se exceptúa el health check para que el balanceador pueda sondear por HTTP.
func ForceHTTPS(enabled bool, trust TrustChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/codex" || r.URL.Path == "/healthz" || EsHTTPS(r, trust) {
				next.ServeHTTP(w, r)
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				destino := *r.URL
				destino.Scheme = "https"
				destino.Host = r.Host
				http.Redirect(w, r, destino.String(), http.StatusPermanentRedirect)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Se requiere HTTPS. La petición se rechazó porque viajó sin cifrar."}`))
		})
	}
}

// SecurityHeaders añade las cabeceras de endurecimiento del navegador.
//
// HSTS solo se manda cuando la petición LLEGÓ por https. Mandarlo por HTTP es
// inútil (el navegador lo ignora por spec) y peligroso en desarrollo: fijaría
// localhost como "solo https" durante un año en el navegador del desarrollador.
//
// La CSP se aplica únicamente al panel estático. Las respuestas de la API son
// JSON y no ejecutan nada, pero se les manda `default-src 'none'` para que, si
// alguna acabara renderizándose, no pueda cargar ni ejecutar recursos.
func SecurityHeaders(prod bool, trust TrustChecker, esPanel func(string) bool) func(http.Handler) http.Handler {
	const cspPanel = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " + // los bundlers inyectan estilos inline
		"img-src 'self' data: blob:; " +
		"media-src 'self' blob:; " +
		"font-src 'self' data:; " +
		"connect-src 'self' wss: https:; " +
		"object-src 'none'; " +
		"base-uri 'none'; " + // impide que un <base> inyectado redirija los assets
		"form-action 'self'; " +
		"frame-ancestors 'none'; " + // clickjacking (versión moderna de X-Frame-Options)
		"upgrade-insecure-requests"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=(), interest-cohort=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("X-Permitted-Cross-Domain-Policies", "none")
			h.Del("Server")
			h.Del("X-Powered-By")

			if esPanel != nil && esPanel(r.URL.Path) {
				h.Set("Content-Security-Policy", cspPanel)
			} else if strings.HasPrefix(r.URL.Path, "/api") {
				h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; sandbox")
			}

			// HSTS: una vez visto, el navegador se niega a hablar HTTP con este
			// dominio durante un año, incluso si el usuario teclea http:// o pincha
			// un enlace manipulado. Cierra la ventana de la PRIMERA petición, que
			// es donde ataca sslstrip.
			if EsHTTPS(r, trust) {
				if prod {
					h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
				} else {
					h.Set("Strict-Transport-Security", "max-age=86400")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimit corta el cuerpo de las peticiones a /api en maxBytes. Sin esto, un
// solo POST con un cuerpo enorme puede agotar la memoria del proceso (DoS
// trivial y gratis para el atacante). Las subidas de adjuntos aplican su propio
// límite, más alto, dentro de su handler.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if maxBytes <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && strings.HasPrefix(r.URL.Path, "/api") {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecretoIgual compara dos secretos en TIEMPO CONSTANTE.
//
// El `==` de Go corta en el primer byte distinto, así que el tiempo de respuesta
// filtra cuántos caracteres del principio acertó el atacante. Con suficientes
// intentos medidos, una API key se reconstruye byte a byte sin conocerla. Esta
// versión siempre tarda lo mismo.
//
// El chequeo previo de longitud no filtra nada útil: la longitud de una API key
// no es secreta y evita comparar contra un slice vacío.
func SecretoIgual(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// OriginChecker construye el validador de Origin para los upgrades de WebSocket.
//
// El `CheckOrigin: return true` de gorilla acepta el upgrade venga de donde
// venga. Aquí la autenticación es por ticket/API key en la URL (no por cookie),
// así que el riesgo clásico de secuestro entre orígenes es bajo — pero dejarlo
// abierto permite que cualquier página web abra conexiones contra el servidor y
// consuma sus recursos, y convierte cualquier filtración futura de un ticket en
// algo explotable desde el navegador de la víctima.
//
// Reglas: sin cabecera Origin se acepta (clientes nativos —la app móvil— no la
// envían); con Origin, debe estar en la lista. Si la lista está vacía no se
// filtra, para no romper una instalación mal configurada.
func OriginChecker(permitidos []string) func(*http.Request) bool {
	set := make(map[string]struct{}, len(permitidos))
	for _, o := range permitidos {
		if o = strings.TrimSpace(strings.ToLower(strings.TrimSuffix(o, "/"))); o != "" {
			set[o] = struct{}{}
		}
	}
	return func(r *http.Request) bool {
		if len(set) == 0 {
			return true
		}
		origin := strings.TrimSpace(strings.ToLower(strings.TrimSuffix(r.Header.Get("Origin"), "/")))
		if origin == "" {
			return true // cliente nativo (app móvil), no un navegador
		}
		_, ok := set[origin]
		return ok
	}
}

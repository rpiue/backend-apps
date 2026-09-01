package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Valores por defecto que SOLO sirven para desarrollo. Si alguno sobrevive a un
// arranque en producción, el sistema entero queda comprometido: quien lea este
// repositorio (o el docker-compose) conoce la contraseña del panel y puede
// firmar sus propios JWT de admin. Por eso el arranque se aborta.
var inseguros = map[string]string{
	"change_me":               "valor de plantilla",
	"admin1234":               "contraseña de ejemplo",
	"CambiarEstePassword123!": "contraseña de ejemplo",
	"chat_app_password":       "contraseña de ejemplo de la BD",
	"apiyc":                   "credencial de ejemplo de la BD",
	"secret":                  "valor trivial",
	"changeme":                "valor de plantilla",
}

// longitudMinimaSecreto es la longitud mínima aceptable para un secreto de
// firma. Con menos de 32 caracteres un HS256 es fuerza-brutable fuera de línea:
// basta capturar UN token para recuperar la clave y luego falsificar cualquier
// sesión de admin.
const longitudMinimaSecreto = 32

// Problema es un hallazgo de configuración.
type Problema struct {
	Clave   string
	Detalle string
	Fatal   bool // true = impide arrancar en producción
}

func (p Problema) String() string { return fmt.Sprintf("%s: %s", p.Clave, p.Detalle) }

// Validar revisa la configuración y devuelve los problemas encontrados.
// En desarrollo son solo advertencias; en producción los Fatal abortan.
func (c *Config) Validar() []Problema {
	var out []Problema
	fatal := func(clave, detalle string) { out = append(out, Problema{clave, detalle, true}) }
	aviso := func(clave, detalle string) { out = append(out, Problema{clave, detalle, false}) }

	// --- Secretos de firma y credenciales ---
	revisarSecreto := func(clave, valor string, minimo bool) {
		v := strings.TrimSpace(valor)
		if v == "" {
			fatal(clave, "está vacío")
			return
		}
		if motivo, ok := inseguros[v]; ok {
			fatal(clave, "sigue con el "+motivo+" ("+v+"); genéralo con ./start.sh secrets")
			return
		}
		if minimo && len(v) < longitudMinimaSecreto {
			fatal(clave, fmt.Sprintf("solo tiene %d caracteres; usa al menos %d (./start.sh secrets)", len(v), longitudMinimaSecreto))
		}
	}

	revisarSecreto("JWT_SECRET", c.JWTSecret, true)
	revisarSecreto("CHAT_JWT_SECRET", c.ChatJWTSecret, true)
	revisarSecreto("ADMIN_API_KEY", c.AdminAPIKey, true)
	revisarSecreto("ADMIN_PASSWORD", c.AdminPassword, false)
	revisarSecreto("CHAT_ADMIN_PASSWORD", c.ChatAdminPassword, false)

	// JWT_SECRET y CHAT_JWT_SECRET compartidos: un token del chat valdría para
	// el panel de admin y al revés. Deben ser claves distintas.
	if c.JWTSecret != "" && c.JWTSecret == c.ChatJWTSecret {
		aviso("CHAT_JWT_SECRET", "es idéntico a JWT_SECRET; usa claves distintas para aislar las sesiones del chat de las del panel")
	}

	// Clave de firma del acceso offline: sin ella se genera una efímera en cada
	// arranque y todas las licencias emitidas dejan de validar al reiniciar.
	if strings.TrimSpace(c.AccessSigningKey) == "" && strings.TrimSpace(c.AccessSigningKeyFile) == "" {
		aviso("ACCESS_SIGNING_KEY", "vacío: se usará la clave persistida en el directorio de secretos; asegúrate de que ese volumen sea permanente o los accesos ya emitidos dejarán de verificar")
	}

	// --- Transporte: esto es lo que cierra el MitM ---
	if !c.ForceHTTPS {
		aviso("FORCE_HTTPS", "desactivado: el API acepta tráfico en texto plano y los tokens viajan interceptables")
	}
	for _, o := range c.CORSOrigins {
		if strings.HasPrefix(strings.ToLower(o), "http://") && !esLocal(o) {
			fatal("CORS_ORIGINS", "incluye un origen público sin TLS ("+o+"); el navegador enviaría los tokens en claro")
		}
	}
	if u, err := url.Parse(c.Dominio); err == nil && u.Scheme == "http" && !esLocal(c.Dominio) {
		fatal("DOMINIO", "usa http:// ("+c.Dominio+"); los enlaces de pago que se generan viajarían sin cifrar")
	}
	for clave, valor := range map[string]string{"BTCPAY_URL": c.BTCPayURL, "NVIDIA_BASE_URL": c.NvidiaBaseURL} {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(valor)), "http://") && !esLocal(valor) {
			fatal(clave, "apunta a un host público por http:// ("+valor+"); la API key viajaría en claro")
		}
	}

	// --- Base de datos ---
	if u, err := url.Parse(c.PostgresURL); err == nil {
		host := u.Hostname()
		if strings.Contains(c.PostgresURL, "sslmode=disable") && !esHostLocal(host) && !esHostPrivado(host) {
			fatal("DATABASE_URL", "usa sslmode=disable contra un host remoto ("+host+"); la conexión a Postgres viaja sin cifrar")
		}
		if pw, _ := u.User.Password(); pw != "" {
			if motivo, ok := inseguros[pw]; ok {
				fatal("DATABASE_URL", "la contraseña de Postgres es un "+motivo)
			}
		}
	}

	// --- Proxies confiables ---
	// Si la lista está vacía, ClientIP siempre usa la IP del socket: detrás de un
	// proxy eso significa que TODO el tráfico comparte la IP del proxy y el rate
	// limit global se agota para todos a la vez.
	if len(c.TrustedProxies) == 0 {
		aviso("TRUSTED_PROXIES", "vacío: si hay un proxy delante, el rate limit contará a todos los clientes como una sola IP")
	}

	return out
}

// esLocal indica si una URL apunta a la máquina local (donde no hay red que
// interceptar, así que http:// es aceptable en desarrollo).
func esLocal(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return esHostLocal(u.Hostname())
}

func esHostLocal(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "" || strings.HasSuffix(h, ".localhost")
}

// esHostPrivado cubre los nombres de servicio de Docker y las redes internas:
// ahí el tráfico no sale del host, así que sslmode=disable no es interceptable
// desde fuera (aunque sigue siendo mejor cifrarlo).
func esHostPrivado(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	if strings.Contains(h, ".") {
		return strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "192.168.") || strings.HasPrefix(h, "172.")
	}
	return h != "" // nombre corto sin puntos = servicio de la red interna de Docker
}

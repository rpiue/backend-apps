// Package middleware contiene middlewares HTTP transversales. ratelimit.go
// implementa rate limiting con ventana fija sobre Redis, para frenar ataques
// DoS y spam (peticiones, códigos de pago, mensajes de chat) sin depender de
// estado en memoria (sobrevive reinicios y escala horizontalmente).
package middleware

import (
	"context"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"codex/backend/internal/cache"
)

// Limiter aplica rate limiting con ventana fija (INCR+EXPIRE) sobre Redis.
type Limiter struct {
	rdb     *redis.Client
	trusted []*net.IPNet
}

// NewLimiter construye el limitador. trustedCIDRs son los proxies de los que SÍ
// aceptamos cabeceras de IP (X-Forwarded-For / cf-connecting-ip).
func NewLimiter(c *cache.Cache, trustedCIDRs []string) *Limiter {
	var nets []*net.IPNet
	for _, cidr := range trustedCIDRs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(cidr)); err == nil {
			nets = append(nets, n)
		}
	}
	var rdb *redis.Client
	if c != nil {
		rdb = c.Client()
	}
	return &Limiter{rdb: rdb, trusted: nets}
}

// ClientIP devuelve la IP real del cliente, honrando cabeceras de proxy SOLO si
// el peer inmediato es un proxy confiable. Así un cliente no puede falsificar su
// IP (mandando un X-Forwarded-For) para esquivar el rate limit.
func (l *Limiter) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && l.isTrusted(peer) {
		if cf := strings.TrimSpace(r.Header.Get("cf-connecting-ip")); cf != "" {
			return cf
		}
		if fwd := r.Header.Get("x-forwarded-for"); fwd != "" {
			if first := strings.TrimSpace(strings.Split(fwd, ",")[0]); first != "" {
				return first
			}
		}
	}
	return host
}

// ClientCountry devuelve el país (ISO-2, ej. "PE") que Cloudflare adjunta en la
// cabecera CF-IPCountry, SOLO si la petición llega desde un proxy confiable
// (evita que un cliente que golpee el origin directamente falsifique el país).
// Devuelve "" si no se puede determinar.
func (l *Limiter) ClientCountry(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && l.isTrusted(peer) {
		return strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	}
	return ""
}

func (l *Limiter) isTrusted(ip net.IP) bool {
	for _, n := range l.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Limit crea un middleware que permite `limit` peticiones por `window` para cada
// (name, IP). Responde 429 con Retry-After. Fail-open si Redis no está.
func (l *Limiter) Limit(name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit <= 0 || l.rdb == nil {
				next.ServeHTTP(w, r)
				return
			}
			ok, retry := l.Allow(r.Context(), name, l.ClientIP(r), limit, window)
			if !ok {
				tooMany(w, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Allow aplica la cuenta de ventana fija para una clave arbitraria (name+key).
// Devuelve (permitido, segundos hasta reintentar). Reutilizable a nivel de
// handler para límites por dispositivo/email (p.ej. brute-force de códigos).
func (l *Limiter) Allow(ctx context.Context, name, key string, limit int, window time.Duration) (bool, int) {
	if limit <= 0 || l.rdb == nil {
		return true, 0
	}
	rkey := "rl:" + name + ":" + key
	cnt, err := l.rdb.Incr(ctx, rkey).Result()
	if err != nil {
		// Fail-open: no tumbamos el servicio si Redis falla; solo lo registramos.
		log.Printf("[ratelimit] redis error (%s): %v", name, err)
		return true, 0
	}
	// Fija la expiración en la primera petición de la ventana (o si se perdió).
	if cnt == 1 {
		_ = l.rdb.Expire(ctx, rkey, window).Err()
	} else if ttl, _ := l.rdb.TTL(ctx, rkey).Result(); ttl < 0 {
		_ = l.rdb.Expire(ctx, rkey, window).Err()
	}
	if cnt > int64(limit) {
		retry := int(window.Seconds())
		if ttl, err := l.rdb.TTL(ctx, rkey).Result(); err == nil && ttl > 0 {
			retry = int(ttl.Seconds()) + 1
		}
		return false, retry
	}
	return true, 0
}

func tooMany(w http.ResponseWriter, retry int) {
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	// Mensaje genérico: no revela IP ni detalles internos.
	_, _ = w.Write([]byte(`{"error":"Demasiadas solicitudes. Intenta de nuevo en un momento."}`))
}

// IsTrustedPeer indica si la conexión viene de un proxy de confianza. Lo usan
// los middlewares de seguridad para decidir si pueden creerse la cabecera
// X-Forwarded-Proto (si no, un cliente podría afirmar "vengo por https" y
// esquivar el bloqueo de texto plano).
func (l *Limiter) IsTrustedPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && l.isTrusted(ip)
}

// Reset borra el contador de (name, key). Se usa tras una autenticación
// correcta: si no, a quien se equivocó de contraseña un par de veces le seguiría
// contando el cupo gastado durante toda la ventana aunque ya haya entrado bien.
func (l *Limiter) Reset(ctx context.Context, name, key string) {
	if l.rdb == nil {
		return
	}
	_ = l.rdb.Del(ctx, "rl:"+name+":"+key).Err()
}

// --- Primitivas para bloqueo progresivo por cuenta ---
//
// El contador de ventana fija de Limit/Allow sirve para frenar el ritmo, pero no
// para defender una credencial corta: al terminar cada ventana el cupo se
// renueva entero, así que quien tenga paciencia sigue probando indefinidamente.
// Para eso hacen falta tres cosas que estas funciones aportan: contar FALLOS
// acumulados, bloquear durante un tiempo que crece con ellos, y poder consultar
// el estado sin modificarlo.

// FailKey compone la clave de fallos de una cuenta.
func failKey(name, key string) string { return "fail:" + name + ":" + key }
func lockKey(name, key string) string { return "lock:" + name + ":" + key }

// RegistrarFallo suma uno al contador de fallos de (name, key) y devuelve el
// total acumulado. El contador caduca a las `ttl` desde el ÚLTIMO fallo: así,
// quien deja de intentarlo se le olvida solo, pero quien insiste ve crecer su
// cuenta sin que se reinicie a mitad de camino.
func (l *Limiter) RegistrarFallo(ctx context.Context, name, key string, ttl time.Duration) int {
	if l.rdb == nil {
		return 0
	}
	k := failKey(name, key)
	n, err := l.rdb.Incr(ctx, k).Result()
	if err != nil {
		log.Printf("[ratelimit] no se pudo registrar el fallo (%s): %v", name, err)
		return 0
	}
	// Se refresca la caducidad en CADA fallo (no solo en el primero), para que la
	// ventana se mida desde el último intento y no desde el primero.
	_ = l.rdb.Expire(ctx, k, ttl).Err()
	return int(n)
}

// Fallos devuelve cuántos fallos acumula la clave, sin modificar nada.
func (l *Limiter) Fallos(ctx context.Context, name, key string) int {
	if l.rdb == nil {
		return 0
	}
	n, err := l.rdb.Get(ctx, failKey(name, key)).Int()
	if err != nil {
		return 0
	}
	return n
}

// LimpiarFallos borra el contador y el bloqueo. Se llama tras un acceso correcto.
func (l *Limiter) LimpiarFallos(ctx context.Context, name, key string) {
	if l.rdb == nil {
		return
	}
	_ = l.rdb.Del(ctx, failKey(name, key), lockKey(name, key)).Err()
}

// Bloquear deja la clave bloqueada durante d. Si ya estaba bloqueada por más
// tiempo, respeta el bloqueo más largo: un bloqueo nuevo nunca acorta uno vigente.
func (l *Limiter) Bloquear(ctx context.Context, name, key string, d time.Duration) {
	if l.rdb == nil || d <= 0 {
		return
	}
	k := lockKey(name, key)
	if ttl, err := l.rdb.TTL(ctx, k).Result(); err == nil && ttl > d {
		return
	}
	_ = l.rdb.Set(ctx, k, "1", d).Err()
}

// SegundosBloqueado devuelve cuántos segundos quedan de bloqueo (0 si no lo está).
func (l *Limiter) SegundosBloqueado(ctx context.Context, name, key string) int {
	if l.rdb == nil {
		return 0
	}
	ttl, err := l.rdb.TTL(ctx, lockKey(name, key)).Result()
	if err != nil || ttl <= 0 {
		return 0
	}
	return int(ttl.Seconds()) + 1
}

package handlers

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Protección contra fuerza bruta por CUENTA para la API principal.
//
// Es el mismo planteamiento que el del chat (internal/chat/authlimit.go), y por
// el mismo motivo: el PIN es de 6 dígitos —un millón de combinaciones— y la IP
// no sirve como eje de la defensa porque el atacante la cambia cuando quiera.
// Lo que no cambia es la cuenta a la que apunta.
//
// La diferencia con el límite por IP que ya había en la ruta (RL_LOGIN_PER_MIN)
// es que aquel solo frena el ritmo: cada minuto renueva el cupo entero, así que
// quien tenga paciencia sigue probando indefinidamente. Aquí los fallos se
// acumulan y el bloqueo crece con ellos.

// escalonesAuth: a partir de N fallos, cuánto se bloquea la cuenta.
// El techo de 1 hora es deliberado — ver el comentario de chat/authlimit.go:
// un bloqueo permanente por cuenta permitiría a cualquiera dejar fuera a otro
// usuario fallando adrede contra su email.
var escalonesAuth = []struct {
	fallos  int
	bloqueo time.Duration
}{
	{20, time.Hour},
	{15, 30 * time.Minute},
	{10, 15 * time.Minute},
	{7, 5 * time.Minute},
	{5, time.Minute},
}

// memoriaFallosAuth es cuánto se recuerda un fallo desde el último intento.
const memoriaFallosAuth = 12 * time.Hour

func bloqueoAuthPara(n int) time.Duration {
	for _, e := range escalonesAuth {
		if n >= e.fallos {
			return e.bloqueo
		}
	}
	return 0
}

// cuentaBloqueada comprueba el bloqueo ANTES de validar la credencial y, si
// procede, responde 429. Devuelve true si hay que cortar aquí.
//
// Va antes de la validación a propósito: comprobar primero la credencial
// significaría hacer la consulta cara (Firebase, bcrypt) en cada intento, lo que
// de por sí es una forma barata de castigar al servidor. Esta comprobación es
// una lectura de Redis.
func (h *Handler) cuentaBloqueada(w http.ResponseWriter, r *http.Request, ambito, cuenta string) bool {
	if h.Limiter == nil {
		return false
	}
	clave := normalizarCuentaAuth(cuenta)
	if clave == "" {
		return false
	}
	if seg := h.Limiter.SegundosBloqueado(r.Context(), ambito, clave); seg > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(seg))
		// Mensaje idéntico al de credencial incorrecta en lo esencial: no se
		// revela si la cuenta existe ni cuántos intentos quedan.
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "Demasiados intentos fallidos. Espera unos minutos antes de volver a intentarlo.",
		})
		return true
	}
	return false
}

// registrarFalloCuenta suma un fallo y bloquea si toca.
func (h *Handler) registrarFalloCuenta(r *http.Request, ambito, cuenta string) {
	if h.Limiter == nil {
		return
	}
	clave := normalizarCuentaAuth(cuenta)
	if clave == "" {
		return
	}
	ctx := r.Context()
	n := h.Limiter.RegistrarFallo(ctx, ambito, clave, memoriaFallosAuth)
	if d := bloqueoAuthPara(n); d > 0 {
		h.Limiter.Bloquear(ctx, ambito, clave, d)
		log.Printf("[auth] cuenta %s bloqueada %s tras %d fallos (%s) desde %s",
			ofuscarCuenta(clave), d, n, ambito, h.Limiter.ClientIP(r))
	}
}

// limpiarFallosCuenta borra los contadores tras un acceso correcto.
func (h *Handler) limpiarFallosCuenta(r *http.Request, ambito, cuenta string) {
	if h.Limiter == nil {
		return
	}
	if clave := normalizarCuentaAuth(cuenta); clave != "" {
		h.Limiter.LimpiarFallos(r.Context(), ambito, clave)
	}
}

func normalizarCuentaAuth(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// ofuscarCuenta recorta el email para los logs: identifica la cuenta durante un
// incidente sin dejar la lista completa de correos en los archivos.
func ofuscarCuenta(email string) string {
	i := strings.Index(email, "@")
	if i <= 0 {
		if len(email) <= 2 {
			return "***"
		}
		return email[:2] + "***"
	}
	if i <= 2 {
		return "***" + email[i:]
	}
	return email[:2] + "***" + email[i:]
}

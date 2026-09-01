package chat

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Protección contra fuerza bruta, centrada en LA CUENTA.
//
// El PIN de un usuario es de 6 dígitos: un millón de combinaciones. Suena a
// mucho, pero probar un millón de veces es trivial para una máquina si nada se
// lo impide. La cuenta es el eje correcto de la defensa porque es lo que el
// atacante ataca: la IP la cambia cuando quiera —un móvil solo tiene que activar
// y desactivar los datos— mientras que el email de la víctima es fijo. Un límite
// por IP, solo, se esquiva repartiendo el ataque; uno por cuenta, no.
//
// La diferencia con un contador de ventana fija: ese renueva el cupo entero cada
// vez que pasa la ventana, así que quien tenga paciencia sigue probando para
// siempre. Aquí los fallos se ACUMULAN y el bloqueo CRECE con ellos.
//
// Con estos tramos, un atacante consigue unos 20 intentos la primera hora y
// luego unos pocos por hora. Recorrer un millón de PIN pasa de horas a siglos.

// escalon define cuánto se bloquea la cuenta a partir de cierto número de fallos.
type escalon struct {
	fallos  int
	bloqueo time.Duration
}

// escalones va del más severo al más leve; se aplica el primero que se cumpla.
//
// El techo es de 1 hora a propósito, y merece explicación: un bloqueo por cuenta
// se puede volver contra el propio usuario. Cualquiera que sepa un email puede
// fallar adrede para dejar a esa persona fuera de su cuenta — cambiar el ataque
// de "robo" a "denegación de servicio". Un bloqueo permanente convertiría eso en
// un problema serio y una llamada a soporte; uno de una hora que se renueva
// mientras el ataque siga hace inviable adivinar el PIN sin dejar a nadie
// bloqueado para siempre. Es el equilibrio deliberado entre las dos amenazas.
var escalones = []escalon{
	{fallos: 20, bloqueo: 1 * time.Hour},
	{fallos: 15, bloqueo: 30 * time.Minute},
	{fallos: 10, bloqueo: 15 * time.Minute},
	{fallos: 7, bloqueo: 5 * time.Minute},
	{fallos: 5, bloqueo: 1 * time.Minute},
}

const (
	// memoriaFallos es cuánto se recuerda un fallo desde el ÚLTIMO intento. Se
	// refresca en cada fallo: quien insiste no consigue que se le olvide.
	memoriaFallos = 12 * time.Hour

	// intentosPorIP es el límite secundario. No es la defensa principal, pero
	// ataja al que prueba UN PIN contra MUCHAS cuentas distintas (spraying), que
	// es el ataque que un contador por cuenta no ve venir: cada cuenta recibe un
	// solo intento y ninguna llega a bloquearse.
	intentosPorIP = 30
	ventanaIP     = 10 * time.Minute
)

// bloqueoPara devuelve cuánto hay que bloquear con n fallos acumulados.
func bloqueoPara(n int) time.Duration {
	for _, e := range escalones {
		if n >= e.fallos {
			return e.bloqueo
		}
	}
	return 0
}

// permitirIntentoAuth se llama ANTES de comprobar la credencial.
//
// El orden importa: comprobar primero la contraseña y contar después dejaría
// pasar el trabajo caro (bcrypt, ~100 ms) en cada petición, lo que por sí solo
// es una forma barata de tumbar el servidor. Aquí la comprobación es una lectura
// de Redis, así que un intento bloqueado se descarta sin gastar casi nada.
func (s *Service) permitirIntentoAuth(w http.ResponseWriter, r *http.Request, ambito, cuenta string) bool {
	if s.lim == nil {
		return true
	}
	ctx := r.Context()
	clave := normalizarCuenta(cuenta)

	// 1) ¿La cuenta está bloqueada ahora mismo?
	if clave != "" {
		if seg := s.lim.SegundosBloqueado(ctx, ambito, clave); seg > 0 {
			s.rechazarAuth(w, seg)
			return false
		}
	}

	// 2) Límite secundario por IP, contra el barrido de muchas cuentas.
	if ok, retry := s.lim.Allow(ctx, "auth:ip:"+ambito, s.clientIP(r), intentosPorIP, ventanaIP); !ok {
		s.rechazarAuth(w, retry)
		return false
	}
	return true
}

// registrarFalloAuth se llama cuando la credencial ha resultado INCORRECTA.
//
// Se cuentan los fallos, no los intentos: a quien entra bien no se le gasta
// cupo, y el contador refleja exactamente lo que interesa vigilar —cuántas veces
// se ha fallado contra esta cuenta.
func (s *Service) registrarFalloAuth(r *http.Request, ambito, cuenta string) {
	if s.lim == nil {
		return
	}
	clave := normalizarCuenta(cuenta)
	if clave == "" {
		return
	}
	ctx := r.Context()
	n := s.lim.RegistrarFallo(ctx, ambito, clave, memoriaFallos)
	if d := bloqueoPara(n); d > 0 {
		s.lim.Bloquear(ctx, ambito, clave, d)
		// Se deja constancia para poder detectar un ataque en curso mirando los
		// logs, sin tener que consultar Redis a mano.
		log.Printf("[auth] cuenta %q bloqueada %s tras %d fallos (%s) desde %s",
			ofuscar(clave), d, n, ambito, s.clientIP(r))
	} else if n >= 3 {
		log.Printf("[auth] %d fallos acumulados en %q (%s) desde %s",
			n, ofuscar(clave), ambito, s.clientIP(r))
	}
}

// limpiarIntentosAuth borra fallos y bloqueo tras un acceso correcto, para no
// arrastrar el cupo gastado de quien simplemente se equivocó un par de veces.
func (s *Service) limpiarIntentosAuth(r *http.Request, ambito, cuenta string) {
	if s.lim == nil {
		return
	}
	if clave := normalizarCuenta(cuenta); clave != "" {
		s.lim.LimpiarFallos(r.Context(), ambito, clave)
	}
}

// DesbloquearCuenta levanta el bloqueo. NO se expone por HTTP a propósito: un
// endpoint que diga cuántas veces se ha intentado entrar en una cuenta es, para
// quien lo alcance, un panel de progreso del ataque, y uno que desbloquee es una
// forma de anular la defensa. Se opera desde el servidor con:
//
//	./start.sh desbloquear correo@ejemplo.com
//
// (ver cmd/desbloquear), que habla con Redis directamente y no abre ninguna ruta.
func (s *Service) DesbloquearCuenta(ctx context.Context, ambito, cuenta string) {
	if s.lim == nil {
		return
	}
	if clave := normalizarCuenta(cuenta); clave != "" {
		s.lim.LimpiarFallos(ctx, ambito, clave)
	}
}

// rechazarAuth responde 429 sin decir si saltó el contador de la cuenta o el de
// la IP, ni cuántos intentos quedan: eso le confirmaría al atacante que el email
// existe y le diría cómo repartir mejor el ataque.
func (s *Service) rechazarAuth(w http.ResponseWriter, retry int) {
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	writeErr(w, http.StatusTooManyRequests,
		"Demasiados intentos fallidos. Espera unos minutos antes de volver a intentarlo.")
}

// normalizarCuenta unifica mayúsculas y espacios para que "A@b.com " y "a@b.com"
// compartan contador y no den dos cupos por la misma cuenta.
func normalizarCuenta(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// ofuscar recorta el email para el log: basta para reconocer la cuenta durante
// un incidente, sin dejar la lista completa de correos en los archivos de log.
func ofuscar(email string) string {
	i := strings.Index(email, "@")
	if i <= 0 {
		if len(email) <= 2 {
			return "***"
		}
		return email[:2] + "***"
	}
	usuario, dominio := email[:i], email[i:]
	if len(usuario) <= 2 {
		return "***" + dominio
	}
	return usuario[:2] + "***" + dominio
}

package handlers

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"codex/backend/internal/firebase"
	"codex/backend/internal/notify"
)

// POST /api/misCodes — lista los códigos del vendedor (requiere token).
func (h *Handler) misCodes(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Todos los parámetros (email, dato, campo) son obligatorios."})
		return
	}
	lista, err := h.FB.ListarCodigos(r.Context(), b.Email)
	if err != nil {
		log.Printf("[misCodes] error interno: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudieron obtener los códigos."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lista": lista})
}

// POST /api/verificarCode — valida un código sin consumirlo (público).
func (h *Handler) verificarCode(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Codigo string `json:"codigo"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Codigo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Todos los parámetros (codigo, dato, campo) son obligatorios."})
		return
	}
	vcodigo := h.FB.VerificarCodigoOnline(r.Context(), b.Codigo)
	writeJSON(w, http.StatusOK, map[string]any{"vcodigo": vcodigo})
}

// POST /api/activar — canjea un código de activación de 6 caracteres y da acceso
// a la app que el código desbloquea. La app NO viene en el código: se resuelve en
// Postgres. Respuestas genéricas (no revela si el código existe, expiró o se usó).
func (h *Handler) activarCode(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Codigo string `json:"codigo"`
		Code   string `json:"code"`
		Email  string `json:"email"`
		Device string `json:"device"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	code := b.Codigo
	if code == "" {
		code = b.Code
	}
	if code == "" || b.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Faltan datos: código y email."})
		return
	}
	ctx := r.Context()

	// Tres frenos distintos, porque cada uno cubre un hueco del otro:
	//
	//  1. Por EMAIL, acumulativo y con bloqueo creciente. Es el principal: el
	//     dispositivo lo elige el cliente (basta reinstalar o mandar otro id) y
	//     la IP se cambia apagando los datos del móvil, pero canjear un código
	//     exige decir a qué cuenta va, y esa no se puede falsear sin perder el
	//     premio.
	//  2. Por DISPOSITIVO, de ventana fija: atrapa al que prueba muchos códigos
	//     contra cuentas distintas desde la misma instalación.
	//  3. Por IP en la propia ruta (RL_CODE_PER_MIN), ya existente.
	if h.cuentaBloqueada(w, r, "activar", b.Email) {
		return
	}
	if h.Limiter != nil && b.Device != "" {
		if ok, retry := h.Limiter.Allow(ctx, "activar-dev", b.Device, 10, time.Minute); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Demasiados intentos. Intenta más tarde."})
			return
		}
	}

	app, plan, err := h.Store.RedeemActivationCode(ctx, code, b.Email, b.Device)
	if err != nil {
		h.registrarFalloCuenta(r, "activar", b.Email)
		// Genérico: mismo mensaje para inválido/expirado/usado.
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Código inválido o ya utilizado."})
		return
	}
	h.limpiarFallosCuenta(r, "activar", b.Email)

	db, appName := h.FB.Registry.UserDB(app)
	res, err := h.FB.DarPlan(ctx, db, b.Email, plan, firebase.DarPlanOpts{})
	if err != nil || !res.Success {
		// El usuario probablemente aún no creó su cuenta en esta app: reponemos el
		// código para que pueda reintentar tras registrarse.
		_ = h.Store.RevertActivationCode(ctx, code)
		log.Printf("[activar] darPlan no aplicado app=%s: %v", appName, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "No se pudo activar. Crea tu cuenta en la app e inténtalo de nuevo.",
		})
		return
	}

	_ = h.Notifier.NotifyUser(ctx, b.Email, appName, notify.Message{
		Title:     "🎉 Tu plan ha sido activado",
		Body:      "¡Listo! Ahora tienes acceso al plan " + plan + ".",
		ChannelID: "alerts",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plan": plan, "message": "Plan activado correctamente."})
}

// POST /api/consumirCode — consume un código y da acceso (requiere token).
func (h *Handler) consumirCode(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Codigo string `json:"codigo"`
		Code   string `json:"code"`
		Email  string `json:"email"`
		Device string `json:"device"`
		App    string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Codigo == "" {
		b.Codigo = b.Code
	}
	b.App = appOrYape(b.App) // app vacía -> "yape" (como en el backend JS)
	if b.Codigo == "" || b.Email == "" || b.Device == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Todos los parámetros (codigo, dato, campo) son obligatorios."})
		return
	}
	log.Printf("[consumirCode] request app=%q email=%q device=%q codigo=%q", b.App, b.Email, b.Device, b.Codigo)
	ctx := r.Context()

	// Freno de fuerza bruta por CUENTA. El código tiene una parte adivinable y
	// este endpoint dice si acertaste, así que sin límite acumulativo es cuestión
	// de insistir. El email del cliente es el eje: la ruta ya exige token, pero
	// un token se obtiene con credenciales propias, así que el atacante siempre
	// canjea contra SU cuenta — y ahí es donde se le cuenta.
	if h.cuentaBloqueada(w, r, "consumir", b.Email) {
		return
	}
	db, _ := h.FB.Registry.UserDB(b.App)
	log.Printf("[consumirCode] consultando código %q en app=%q db=%q", b.Codigo, b.App, db.ID)
	res, err := h.FB.ConsumirCodigo(ctx, db, b.Codigo, b.Device, b.Email)
	if err != nil {
		log.Printf("[consumirCode] error interno: code=%q app=%q email=%q device=%q err=%v", b.Codigo, b.App, b.Email, b.Device, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo procesar el código."})
		return
	}

	// El JS: cuando darPlan no da acceso, el callback de la transacción sale sin
	// devolver nada y el handler responde 404 con este mensaje. Un Body nil es
	// ese mismo caso.
	if res.Body == nil {
		h.registrarFalloCuenta(r, "consumir", b.Email)
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "Usuario no encontrado o no se pudo actualizar el dato.",
		})
		return
	}

	// ConsumirCodigo responde 200 con ok:false cuando el código no sirve, así que
	// el fallo se detecta ahí y no por el código de estado HTTP.
	if aceptado, _ := res.Body["ok"].(bool); !aceptado {
		h.registrarFalloCuenta(r, "consumir", b.Email)
		writeJSON(w, http.StatusOK, map[string]any{"vcodigo": res.Body, "codigos": []CrossCode{}})
		return
	}
	h.limpiarFallosCuenta(r, "consumir", b.Email)

	// Notificaciones por función directa: al cliente (plan activado) y al vendedor.
	if res.NotifyClient != "" {
		_ = h.Notifier.NotifyUser(ctx, res.NotifyClient, b.App, notify.Message{
			Title: "🎉 Tu plan ha sido activado",
			Body:  "¡Felicidades!. Ahora tienes acceso al plan " + res.Plan + ".", ChannelID: "alerts",
		})
	}
	if res.NotifyOwner != "" {
		_ = h.Notifier.NotifyUser(ctx, res.NotifyOwner, b.App, notify.Message{
			Title: "✅ Código consumido exitosamente",
			Body:  "Un usuario ha activado el plan " + res.Plan + " con éxito usando uno de tus códigos.", ChannelID: "alerts",
		})
	}
	// Acceso cruzado (Yape↔BCP): genera el código para la otra app y lo devuelve
	// al cliente para mostrarlo directamente (sin push).
	codigos := []CrossCode{}
	if res.Plan != "" {
		codigos = h.crossGrant(ctx, b.App, b.Email, res.Plan, false)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vcodigo": res.Body, "codigos": codigos})
}

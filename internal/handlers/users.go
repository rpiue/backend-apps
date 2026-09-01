package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"codex/backend/internal/firebase"
	"codex/backend/internal/genbin"
	"codex/backend/internal/notify"
)

// parseDato replica el `try JSON.parse(dato) catch (texto)` del JS: si el valor
// es un string que contiene JSON válido, lo parsea; si no, lo deja tal cual.
func parseDato(raw any) any {
	s, ok := raw.(string)
	if !ok {
		return raw
	}
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err == nil {
		return parsed
	}
	return s
}

// appOrYape replica el `app || "yape"` del backend JS: si el cliente no manda
// app (o viene en blanco), se asume "yape" por defecto. Se usa en los endpoints
// del CLIENTE para que el app crudo nunca quede vacío en notificaciones, cross-
// grant, métricas o respuestas. (No aplica a filtros del panel admin, donde app
// vacío significa "todas las apps".)
func appOrYape(app string) string {
	if strings.TrimSpace(app) == "" {
		return "yape"
	}
	return app
}

// POST /api/index — autenticación: valida email+pin y devuelve un JWT de 65s.
func (h *Handler) authIndex(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		Pin   any    `json:"pin"`
		App   string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	pin := fmt.Sprintf("%v", b.Pin)
	if b.Email == "" || b.Pin == nil || pin == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Faltan parámetros (email, pin, appName)"})
		return
	}
	ctx := r.Context()
	db, nameApp := h.FB.Registry.UserDB(b.App)

	// Bloqueo por cuenta antes de validar nada (ver authguard.go). El PIN es de
	// 6 dígitos y el límite por IP de la ruta solo frena el ritmo: renueva el
	// cupo cada minuto, así que por sí solo no impide recorrer el espacio.
	if h.cuentaBloqueada(w, r, "index", b.Email) {
		return
	}

	// 1) intentar cache (email -> pin)
	if cached, ok := h.getUserSecret(ctx, nameApp, b.Email); ok && cached.Pin == pin {
		tok, _ := h.Auth.Sign(map[string]any{"email": b.Email, "nameApp": nameApp}, 65*time.Second)
		writeJSON(w, http.StatusOK, map[string]string{"token": tok})
		return
	}

	// 2) validar contra Firebase
	users, err := h.FB.GetUserByEmail(ctx, db, b.Email, &pin)
	if err != nil {
		// Se cuenta el FALLO, no el intento: a quien acierta no se le gasta cupo.
		h.registrarFalloCuenta(r, "index", b.Email)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Email o pin incorrecto"})
		return
	}
	h.limpiarFallosCuenta(r, "index", b.Email)
	uid, _ := users[0]["id"].(string)
	h.saveUserSecret(ctx, nameApp, uid, b.Email, pin)
	tok, _ := h.Auth.Sign(map[string]any{"email": b.Email, "nameApp": nameApp, "uid": uid}, 65*time.Second)
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// POST /api/login — devuelve el/los usuario(s) que coinciden con email+clave.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		Clave any    `json:"clave"`
		App   string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	b.App = appOrYape(b.App) // app vacía -> "yape" (como en el backend JS)
	db, _ := h.FB.Registry.UserDB(b.App)
	var clave *string
	if b.Clave != nil {
		s := fmt.Sprintf("%v", b.Clave)
		clave = &s
	}
	if h.cuentaBloqueada(w, r, "login", b.Email) {
		return
	}
	results, err := h.FB.GetUserByEmail(r.Context(), db, b.Email, clave)
	if err != nil {
		h.registrarFalloCuenta(r, "login", b.Email)
		// El error interno ya no se devuelve tal cual: el mensaje de Firebase
		// puede describir la consulta y sirve de pista sobre la estructura de
		// los datos. Se registra en el log y al cliente le llega algo genérico.
		log.Printf("[login] fallo de credenciales o error de Firebase: %v", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Email o clave incorrecta"})
		return
	}
	h.limpiarFallosCuenta(r, "login", b.Email)
	writeJSON(w, http.StatusOK, map[string]any{"firebaseResponse": results})
}

// POST /api/verificar-disponible — { disponible } (true si email/teléfono libres).
func (h *Handler) verificarDisponible(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email    string `json:"email"`
		Telefono string `json:"telefono"`
		App      string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	db, _ := h.FB.Registry.UserDB(b.App)
	em := strings.ToLower(strings.TrimSpace(b.Email))
	disponible, err := h.FB.ExisteUsuario(r.Context(), db, b.Telefono, em)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disponible": disponible})
}

// POST /api/datosUsuario — datos del usuario por ID (requiere token).
func (h *Handler) datosUsuario(w http.ResponseWriter, r *http.Request) {
	var b struct {
		UserID string `json:"userID"`
		App    string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	ctx := r.Context()
	db, appName := h.FB.Registry.UserDB(b.App)
	data, err := h.FB.GetUserData(ctx, db, b.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	// Si no trae "acceso", agregarlo en false (replica index.js).
	if _, ok := data["acceso"]; !ok {
		data["acceso"] = false
	}
	email, _ := data["email"].(string)
	pin := ""
	if v, ok := data["clave"]; ok {
		pin = fmt.Sprintf("%v", v)
	}
	if email != "" || pin != "" {
		h.saveUserSecret(ctx, appName, b.UserID, email, pin)
	}
	writeJSON(w, http.StatusOK, map[string]any{"firebaseResponse": map[string]any{"data": data}})
}

// updateUser maneja /update y /updateU (misma lógica; difieren solo en el middleware).
func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		Dato  any    `json:"dato"`
		Campo string `json:"campo"`
		App   string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" || b.Dato == nil || b.Campo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Todos los parámetros (email, dato, campo) son obligatorios.",
		})
		return
	}
	db, _ := h.FB.Registry.UserDB(b.App)
	parsed := parseDato(b.Dato)
	if err := h.FB.UpdateUserData(r.Context(), db, b.Email, b.Campo, parsed); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "Usuario no encontrado o no se pudo actualizar el dato.",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":      "Usuario actualizado exitosamente.",
		"updatedField": b.Campo,
		"newValue":     parsed,
	})
}

// POST /api/updateT — alta/actualización masiva de teléfonos (requiere token).
func (h *Handler) updateT(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Dato  []map[string]any `json:"dato"`
		Campo string           `json:"campo"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if len(b.Dato) == 0 || b.Campo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Todos los parámetros (email, dato, campo) son obligatorios.",
		})
		return
	}
	if err := h.FB.UpdateNumbers(r.Context(), b.Dato); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "Usuario no encontrado o no se pudo actualizar el dato.",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":      "Usuario actualizado exitosamente.",
		"updatedField": b.Campo,
		"newValue":     b.Dato,
	})
}

// POST /api/delate — soft-delete del usuario (requiere token).
func (h *Handler) delate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		App   string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Todos los parámetros (email, dato, campo) son obligatorios.",
		})
		return
	}
	db, _ := h.FB.Registry.UserDB(b.App)
	if err := h.FB.EliminarUsuario(r.Context(), db, b.Email); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "Usuario no encontrado o no se pudo actualizar el dato.",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Usuario eliminado exitosamente."})
}

// POST /api/cambiar — cambia el dispositivo del usuario (requiere token).
// NOTA: el JS original llamaba cambiarMovil sin pasar la DB (bug); aquí se corrige
// usando la DB de la app (yape por defecto).
func (h *Handler) cambiar(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email   string `json:"email"`
		IDMovil string `json:"idMovil"`
		App     string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" || b.IDMovil == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Faltan campos requeridos"})
		return
	}
	db, _ := h.FB.Registry.UserDB(b.App)
	s, err := h.FB.CambiarMovil(r.Context(), db, b.Email, b.IDMovil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Error al generar el link de pago"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.Success, "msj": s.Message})
}

// POST /api/negroide — asigna un plan manualmente (requiere token).
func (h *Handler) negroide(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email string `json:"email"`
		Plan  string `json:"plan"`
		App   string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	b.App = appOrYape(b.App) // app vacía -> "yape" (como en el backend JS)
	if b.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Faltan campos requeridos"})
		return
	}
	ctx := r.Context()
	db, _ := h.FB.Registry.UserDB(b.App)
	s, err := h.FB.DarPlan(ctx, db, b.Email, b.Plan, firebase.DarPlanOpts{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Error al generar el link de pago"})
		return
	}
	codigos := []CrossCode{}
	if s.Success && s.NotifyEmail != "" {
		// Notificación por FUNCIÓN directa (antes era un POST HTTP a /api/notify).
		_ = h.Notifier.NotifyUser(ctx, s.NotifyEmail, b.App, notify.Message{
			Title:     "🎉 Tu plan ha sido activado",
			Body:      "¡Felicidades!. Ahora tienes acceso al plan " + s.Plan + ".",
			ChannelID: "alerts",
		})
		// Suma a las métricas (precio estándar del plan) y avisa al panel en vivo.
		h.recordIngreso(ctx, b.Email, s.Plan, 0, b.App, "negroide")
		// Acceso cruzado (Yape↔BCP): genera el código para la otra app (si el correo
		// no existe allí) y lo devuelve al frontend para mostrarlo directamente.
		codigos = h.crossGrant(ctx, b.App, b.Email, s.Plan, false)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.Success, "msj": s.Message, "codigos": codigos})
}

// POST /api/crearUsuario — crea un usuario nuevo (replica el index.js actual:
// para apps != yape genera tarjeta BIN + cuenta bancaria; acepta pin, dispositivo, digt).
func (h *Handler) crearUsuario(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Nombre        string `json:"nombre"`
		Apellidos     string `json:"apellidos"`
		Email         string `json:"email"`
		Telefono      any    `json:"telefono"`
		Saldo         any    `json:"saldo"`
		Pin           any    `json:"pin"`
		CodigoV       any    `json:"codigo_v"`
		Digt          string `json:"digt"`
		App           string `json:"app"`
		Dispositivo   any    `json:"dispositivo"`
		DispositivosA any    `json:"dispositivosA"` // ANDROID_ID que envía la app (huella)
		DeviceID      string `json:"deviceId"`      // alias explícito de la huella
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Nombre == "" || b.Apellidos == "" || b.Email == "" || b.Telefono == nil || b.Saldo == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Todos los campos (nombres, apellidos, email, telefono, saldo) son obligatorios.",
		})
		return
	}

	// --- Control de registro: solo Perú + 1 cuenta/mes por dispositivo e IP ---
	deviceID := strings.TrimSpace(b.DeviceID)
	if deviceID == "" && b.DispositivosA != nil {
		deviceID = strings.TrimSpace(fmt.Sprintf("%v", b.DispositivosA))
	}
	var clientIP, country string
	if h.Limiter != nil {
		clientIP = h.Limiter.ClientIP(r)
		country = h.Limiter.ClientCountry(r)
	}
	// Geo: solo IPs de Perú (país que certifica Cloudflare). Si no se puede
	// determinar el país (sin CF/dev), no se bloquea.
	if !h.Cfg.SignupAllowNonPE && country != "" && country != "PE" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "El registro solo está disponible en Perú.",
		})
		return
	}
	// Límite: 1 cuenta por ventana (mes) por dispositivo O por IP.
	if h.Cfg.SignupLimitDays > 0 && h.Store != nil {
		window := time.Duration(h.Cfg.SignupLimitDays) * 24 * time.Hour
		if n, err := h.Store.CountRecentAccountCreations(r.Context(), deviceID, clientIP, window); err == nil && n > 0 {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "Solo se puede crear una cuenta al mes desde este dispositivo o red.",
			})
			return
		}
	}

	var card map[string]any
	var inicioApp bool
	if b.App != "" && strings.ToLower(b.App) != "yape" {
		banco := genbin.GenerarBanco("BCP", 1)
		inicioApp = strings.ToLower(b.App) == "bcp"
		tarjetas := genbin.GenerarTarjetas(b.Digt, 1)
		if len(tarjetas) > 0 {
			t := tarjetas[0]
			card = map[string]any{
				"tipo": t.Tipo, "numeroCompleto": t.NumeroCompleto, "mes": t.Mes,
				"anio": t.Anio, "ultimosCuatro": t.UltimosCuatro,
			}
			if len(banco) > 0 {
				card["cuentaSimple"] = banco[0].CuentaSimple
				card["cciFormato"] = banco[0].CCIFormato
			}
		}
	}

	db, nameApp := h.FB.Registry.UserDB(b.App)
	em := strings.ToLower(strings.TrimSpace(b.Email))

	usuario := map[string]any{
		"nombre":    b.Nombre,
		"apellidos": b.Apellidos,
		"telefono":  b.Telefono,
		"saldo":     b.Saldo,
		"email":     em,
		"planN":     0,
		"app":       nameApp,
	}
	if b.CodigoV != nil {
		usuario["codigo_v"] = b.CodigoV
	}
	if b.Dispositivo != nil {
		usuario["dispositivos"] = b.Dispositivo
	}
	if inicioApp {
		usuario["inicio"] = inicioApp
	}
	if card != nil {
		usuario["card"] = card
	}
	if b.Pin != nil {
		usuario["clave"] = strings.TrimSpace(fmt.Sprintf("%v", b.Pin))
	}

	id, err := h.FB.GuardarUsuario(r.Context(), db, usuario)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.Store.RegistrarUsuarioMetrica(r.Context(), em, nameApp)
	// Deja constancia para el límite de 1 cuenta/mes por dispositivo e IP.
	if h.Store != nil {
		_ = h.Store.RecordAccountCreation(r.Context(), deviceID, clientIP, country, em, nameApp)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message": "Usuario creado exitosamente.",
		"usuario": usuario,
		"id":      id,
	})
}

// POST /api/telefono — busca nombre por número (requiere token).
func (h *Handler) telefono(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Telefono string `json:"telefono"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Telefono == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"mensaje": `El parámetro "telefono" es obligatorio`})
		return
	}
	nombre, err := h.FB.BuscarNumeroTelefonico(r.Context(), b.Telefono)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "mensaje": "Error interno del servidor", "error": err.Error(),
		})
		return
	}
	if nombre == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "mensaje": "Número no encontrado", "data": nil,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"mensaje": "Número encontrado",
		"data":    map[string]any{"telefono": b.Telefono, "nombreCompleto": nombre},
	})
}

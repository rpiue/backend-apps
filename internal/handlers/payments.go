package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"codex/backend/internal/btcpay"
	"codex/backend/internal/firebase"
	"codex/backend/internal/mercadopago"
	secmw "codex/backend/internal/middleware"
	"codex/backend/internal/notify"
)

// makeUID replica makeUniqueIds(email).uid: sha256(email|iso|rand) base64url[:22].
func makeUID(email string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	raw := strings.ToLower(strings.TrimSpace(email)) + "|" + time.Now().UTC().Format(time.RFC3339Nano) + "|" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

// planInfo clasifica un planNombre en {plan canónico, planN, válido}. El monto que
// devuelve es solo un FALLBACK: el precio real se lee de Firebase (ver
// precioDesdeFirebase). Aun así se conserva por si Firebase no está disponible.
func planInfo(planNombre string) (int, string, int, bool) {
	s := strings.ToLower(planNombre)
	switch {
	case strings.Contains(s, "basico grupal"):
		return 50, "Basico Grupal", 1, true
	case strings.Contains(s, "medium grupal plus"):
		return 130, "Medium Grupal Plus", 3, true
	case strings.Contains(s, "medium grupal"):
		return 85, "Medium Grupal", 2, true
	case strings.Contains(s, "medium"):
		return 35, "Medium", 0, true
	case strings.Contains(s, "basico"):
		return 30, "Basico", 0, true
	}
	return 0, "", 0, false
}

// precioDesdeFirebase busca el precio REAL del plan en las colecciones
// planes/planGrupal de la app (campo `precio`), en vez del hardcode. Empareja
// cada doc por su `titulo` usando la misma clasificación que planInfo, así "Plan
// Basico"→Basico, "Medium Grupal Plus"→Medium Grupal Plus, etc. Devuelve
// (precio, true) solo si encuentra el plan y su precio es válido.
func (h *Handler) precioDesdeFirebase(ctx context.Context, app, planCanonico string) (int, bool) {
	db, _, ok := h.FB.Registry.AppDataDB(app)
	if !ok {
		return 0, false
	}
	data, err := h.FB.GetAppData(ctx, db, nil, nil, nil)
	if err != nil {
		return 0, false
	}
	return precioDeCatalogo(data.Planes, data.PlanGrupal, planCanonico)
}

// precioDeCatalogo busca en las colecciones de planes el doc cuyo `titulo`
// clasifica (vía planInfo) al mismo plan canónico pedido, y devuelve su `precio`.
func precioDeCatalogo(planes, planGrupal []map[string]any, planCanonico string) (int, bool) {
	for _, col := range [][]map[string]any{planes, planGrupal} {
		for _, doc := range col {
			titulo := toStr(doc["titulo"])
			if titulo == "" {
				titulo = toStr(doc["nombre"])
			}
			if _, canon, _, ok := planInfo(titulo); ok && canon == planCanonico {
				if p, ok := parsePrecio(doc["precio"]); ok {
					return p, true
				}
				if p, ok := parsePrecio(doc["monto"]); ok {
					return p, true
				}
			}
		}
	}
	return 0, false
}

// POST /api/sara — genera el link/código de pago para un plan.
func (h *Handler) sara(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email      string `json:"email"`
		PlanNombre string `json:"planNombre"`
		Nombre     string `json:"nombre"`
		App        string `json:"app"`
		BTC        bool   `json:"btc"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Email == "" || b.PlanNombre == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Faltan campos requeridos: email, monto o planNombre"})
		return
	}
	monto, plan, planN, ok := planInfo(b.PlanNombre)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Error al generar el link de pago"})
		return
	}
	app := b.App
	if app == "" {
		app = "yape"
	}
	// Precio REAL desde Firebase (planes/planGrupal). El monto de planInfo queda
	// solo como fallback si Firebase no responde o no tiene ese plan.
	if p, found := h.precioDesdeFirebase(r.Context(), app, plan); found {
		monto = p
	}
	if monto <= 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Error al generar el link de pago"})
		return
	}

	// Rama Bitcoin (btc:true): crea un invoice en BTCPay y devuelve wallet+monto.
	if b.BTC {
		// Debe estar habilitado a nivel servidor Y por el flag por-app del panel.
		if !h.btcDisponibleParaApp(r.Context(), app) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Pagos BTC no disponibles"})
			return
		}
		pago := h.BTC.CrearPago(r.Context(), btcpay.CrearPagoInput{
			Precio: float64(monto) / 3.8, Email: b.Email, App: app, Plan: plan, PlanN: planN,
		})
		if !pago.OK {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "message": "No se pudo generar el link de pago", "error": pago,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "btc": true, "ok": true, "wallet": pago.Wallet, "monto": pago.Monto,
		})
		return
	}

	res, err := h.generarLinkPago(r.Context(), linkInput{
		Email: b.Email, Nombre: b.Nombre, Monto: float64(monto),
		Descripcion: b.PlanNombre, Plan: plan, App: app, PlanN: planN,
	})
	if err != nil || res == nil || res["status"] == false {
		writeJSON(w, http.StatusInternalServerError, saraErrorBody(res))
		return
	}
	out := map[string]any{"success": true}
	for k, v := range res {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/sara?app=yape — el frontend consulta qué métodos de pago hay
// disponibles para su app (por ahora, si acepta BTC). No crea ningún pago.
func (h *Handler) saraConfig(w http.ResponseWriter, r *http.Request) {
	app := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("app")))
	if app == "" {
		app = "yape"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "app": app, "btcDisponible": h.btcDisponibleParaApp(r.Context(), app),
	})
}

// btcDisponibleParaApp indica si una app acepta pago por BTC. Requiere DOS cosas:
//   - que el servidor tenga BTCPay configurado (si no, no hay forma de cobrar), y
//   - que el flag por-app `btcDisponible` (en datos/app) no esté en false.
//
// Si el flag está ausente, por defecto sigue la capacidad del servidor: así no se
// desactiva BTC en apps existentes solo por no tener el campo escrito todavía.
func (h *Handler) btcDisponibleParaApp(ctx context.Context, app string) bool {
	serverBtc := h.BTC != nil && h.BTC.Enabled()
	if !serverBtc {
		return false // sin BTCPay configurado no hay forma de cobrar
	}
	db, _, ok := h.FB.Registry.AppDataDB(app)
	if !ok {
		return false
	}
	doc, exists, err := h.FB.GetDoc(ctx, db, "datos/app")
	var datos map[string]any
	if err == nil && exists {
		datos = doc.Data
	}
	return btcFlagEfectivo(serverBtc, datos)
}

// btcFlagEfectivo decide si BTC está disponible dado el soporte del servidor y el
// doc datos/app (nil si no existe). Con soporte de servidor, el flag por-app solo
// puede DESACTIVARLO explícitamente; si está ausente, por defecto queda activo
// (no se desactiva BTC en apps existentes por no tener el campo escrito).
func btcFlagEfectivo(serverBtc bool, datos map[string]any) bool {
	if !serverBtc {
		return false
	}
	if datos == nil {
		return true
	}
	v, ok := datos["btcDisponible"]
	if !ok {
		return true
	}
	return firebaseBool(v)
}

// firebaseBool normaliza un booleano venido de Firestore (bool o string).
func firebaseBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		s := strings.ToLower(strings.TrimSpace(b))
		return s == "true" || s == "1" || s == "si" || s == "sí"
	}
	return false
}

// saraErrorBody arma la respuesta de error de /sara EXACTAMENTE como index.js:
//
//	{ success:false, message: <detallado o genérico>, error: <res o null> }
//
// El `message` que ve el frontend es el detallado que devuelve generarLinkPago
// (p.ej. "No pudimos leer el código… revisa tu correo"), no uno genérico: la app
// depende de ese texto para guiar al usuario. `error` lleva el objeto original,
// o null si no hubo respuesta (equivalente a `paymentLink || null`).
func saraErrorBody(res map[string]any) map[string]any {
	msg := "No se pudo generar el link de pago"
	if res != nil {
		if m, ok := res["message"].(string); ok && m != "" {
			msg = m
		}
	}
	var errObj any
	if res != nil {
		errObj = res
	}
	return map[string]any{"success": false, "message": msg, "error": errObj}
}

type linkInput struct {
	Email, Nombre, Descripcion, Plan, App string
	Monto                                 float64
	PlanN                                 int
}

// generarLinkPago crea un pago en efectivo (CIP de PagoEfectivo) directamente por
// la API de Pagos de MercadoPago (payment_method_id=pagoefectivo_atm), que devuelve
// el código en la respuesta. Reemplaza al scraper de Playwright, que se rompía por
// la detección de bots y los cambios de la página de checkout de MercadoPago.
// Reusa un CIP pendiente vigente si existe y persiste la compra (Firestore + Postgres).
func (h *Handler) generarLinkPago(ctx context.Context, in linkInput) (map[string]any, error) {
	compraID := makeUID(in.Email)

	// 1) ¿Hay un CIP pendiente vigente para este plan? Reusar su código.
	existente, _, err := h.FB.GetCipPendienteVigente(ctx, in.Email, in.Plan)
	if err == nil && existente != nil && existente.CodigoPago != "" {
		out := map[string]any{"status": true, "codigo": existente.CodigoPago, "message": "S"}
		if existente.Link != "" {
			out["link"] = existente.Link
		}
		return out, nil
	}

	// 2) Crear el pago en efectivo por API: el código viene en la respuesta.
	pago, err := h.MP.CrearPagoEfectivo(ctx, mercadopago.PagoEfectivoInput{
		Email: in.Email, Nombre: in.Nombre, Monto: in.Monto, Descripcion: in.Descripcion,
		Plan: in.Plan, App: in.App, PlanN: in.PlanN, CompraID: compraID,
		NotifyURL: h.Cfg.Dominio + "/api/webhook",
	})
	if err != nil || pago == nil || pago.Codigo == "" {
		log.Printf("[sara] pagoefectivo falló para %s: %v (codigo=%q)", in.Email, err, codigoDe(pago))
		return map[string]any{
			"status":  false,
			"message": "No pudimos generar tu código de pago.\nIntenta de nuevo en unos minutos.",
		}, nil
	}

	// Persistir la compra (Firestore) y el registro local (Postgres, conciliación/recordatorios).
	_ = h.FB.CrearCompra(ctx, in.Email, firebase.Compra{
		ID: compraID, Titulo: orDef(in.Descripcion, in.Plan), Plan: in.Plan,
		Status: "pendiente", CodigoPago: pago.Codigo, Link: pago.Ticket, App: in.App, Monto: int(in.Monto),
	})
	_, _ = h.Store.CrearRegistroPago(ctx, in.Email, pago.Codigo, compraID, in.App, int(in.Monto), in.Plan)

	out := map[string]any{"status": true, "codigo": pago.Codigo}
	if pago.Ticket != "" {
		out["link"] = pago.Ticket // enlace/ticket que genera MercadoPago
	}
	return out, nil
}

// codigoDe devuelve el código de un resultado que puede ser nil (para el log).
func codigoDe(p *mercadopago.PagoEfectivoResult) string {
	if p == nil {
		return ""
	}
	return p.Codigo
}

// POST /api/webhook — confirmación de pago de MercadoPago.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	if !h.validarMPWebhookToken(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var b struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	_ = readJSON(w, r, &b)
	if b.Type == "payment" && b.Data != nil {
		go h.procesarPago(context.Background(), toStr(b.Data["id"]))
	}
	w.WriteHeader(http.StatusOK)
}

// validarMPWebhookToken replica validateMpWebhookToken: si hay token configurado,
// exige que coincida el query ?token= o el header x-webhook-token.
func (h *Handler) validarMPWebhookToken(r *http.Request) bool {
	tok := h.Cfg.MPWebhookToken
	if tok == "" {
		return true
	}
	// Tiempo constante: este endpoint es público y un atacante puede llamarlo
	// tantas veces como quiera para medir tiempos y deducir el token.
	return secmw.SecretoIgual(r.URL.Query().Get("token"), tok) ||
		secmw.SecretoIgual(r.Header.Get("x-webhook-token"), tok)
}

// POST /api/webhook/btcpay — confirmación de pago BTCPay (verifica HMAC sha256).
func (h *Handler) webhookBTCPay(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	sig := r.Header.Get("BTCPay-Sig")
	if sig == "" {
		http.Error(w, "Missing signature", http.StatusBadRequest)
		return
	}
	mac := hmac.New(sha256.New, []byte(h.Cfg.BTCPayWebhookSecret))
	mac.Write(raw)
	computed := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(computed)) {
		http.Error(w, "Invalid signature", http.StatusBadRequest)
		return
	}

	var event struct {
		Type      string `json:"type"`
		InvoiceID string `json:"invoiceId"`
		Invoice   struct {
			Status   string         `json:"status"`
			Metadata map[string]any `json:"metadata"`
		} `json:"invoice"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	// Solo procesar el evento final (InvoiceSettled / status Settled).
	if event.Type != "InvoiceSettled" && event.Invoice.Status != "Settled" {
		_, _ = w.Write([]byte("Ignored"))
		return
	}
	if event.InvoiceID == "" {
		_, _ = w.Write([]byte("No invoiceId"))
		return
	}
	// Dedupe por invoiceId (sobrevive reinicios usando Redis).
	if dup, _ := h.Cache.SetNX(r.Context(), "btcpay:invoice:"+event.InvoiceID, "1", 24*time.Hour); !dup {
		_, _ = w.Write([]byte("Duplicate ignored"))
		return
	}

	metadata := event.Invoice.Metadata
	if metadata == nil {
		metadata = event.Metadata
	}
	if metadata != nil {
		go h.procesarPagoBTC(context.Background(), metadata)
	}
	_, _ = w.Write([]byte("OK"))
}

// procesarPagoBTC aplica el plan/códigos tras un pago BTC confirmado.
func (h *Handler) procesarPagoBTC(ctx context.Context, metadata map[string]any) {
	email := toStr(metadata["email"])
	plan := toStr(metadata["plan"])
	app := toStr(metadata["app"])
	nombre := toStr(metadata["nombre"])
	n := toInt(metadata["planN"])
	monto := toInt(metadata["monto"])

	db, _ := h.FB.Registry.UserDB(app)
	if n != 0 {
		h.otorgarGrupal(ctx, db, app, email, nombre, n)
	} else {
		if s, err := h.FB.DarPlan(ctx, db, email, plan, firebase.DarPlanOpts{}); err == nil && s.Success {
			_ = h.Notifier.NotifyUser(ctx, email, app, notify.Message{
				Title: "🎉 Tu plan ha sido activado",
				Body:  "¡Felicidades!. Ahora tienes acceso al plan " + plan + ".", ChannelID: "alerts",
			})
			// Acceso cruzado (Yape↔BCP) tras el pago BTC confirmado (async → push).
			_ = h.crossGrant(ctx, app, email, plan, true)
		}
	}
	h.recordIngreso(ctx, email, plan, monto, app, "pago")
	_ = h.Store.RegistrarSuscripcionMeses(ctx, email, plan, monto, 1, app)
}

// otorgarGrupal aplica un plan grupal ya pagado: genera/renueva los códigos del
// vendedor en controlPagos y avisa por push tanto a él como a los clientes cuyo
// acceso se renovó (en JS ese aviso salía de darPlan, que aquí no notifica).
func (h *Handler) otorgarGrupal(ctx context.Context, db firebase.Project, app, email, nombre string, n int) {
	res, err := h.FB.GenerarCodigosParaUsuario(ctx, db, email, nombre, n)
	if err != nil || !res.Success {
		return
	}
	_ = h.Notifier.NotifyUser(ctx, email, app, notify.Message{
		Title: "🎉 Tu plan ha sido activado",
		Body:  "¡Felicidades!. Ahora tienes acceso al plan " + res.Plan + ".", ChannelID: "alerts",
	})
	for _, correo := range res.Beneficiarios {
		_ = h.Notifier.NotifyUser(ctx, correo, app, notify.Message{
			Title: "🎉 Tu plan ha sido activado",
			Body:  "¡Felicidades!. Ahora tienes acceso al plan " + res.Plan + ".", ChannelID: "alerts",
		})
	}
}

func (h *Handler) procesarPago(ctx context.Context, paymentID string) {
	if paymentID == "" {
		return
	}
	pago, err := h.MP.GetPayment(ctx, paymentID)
	if err != nil || pago.Status != "approved" {
		return
	}
	email := pago.Payer.Email
	plan := toStr(pago.Metadata["plan"])
	monto := toInt(pago.Metadata["monto"])
	app := toStr(pago.Metadata["app"])
	compraID := toStr(pago.Metadata["compraId"])
	nombre := toStr(pago.Metadata["nombre"])
	n := toInt(pago.Metadata["planN"])

	db, _ := h.FB.Registry.UserDB(app)

	if n != 0 {
		// Plan grupal: genera códigos para el vendedor.
		h.otorgarGrupal(ctx, db, app, email, nombre, n)
	} else {
		if s, err := h.FB.DarPlan(ctx, db, email, plan, firebase.DarPlanOpts{}); err == nil && s.Success {
			_ = h.Notifier.NotifyUser(ctx, email, app, notify.Message{
				Title: "🎉 Tu plan ha sido activado",
				Body:  "¡Felicidades!. Ahora tienes acceso al plan " + plan + ".", ChannelID: "alerts",
			})
			// Acceso cruzado (Yape↔BCP) tras el pago aprobado por MercadoPago (async → push).
			_ = h.crossGrant(ctx, app, email, plan, true)
		}
	}

	// Pago real aprobado: suma el monto efectivamente pagado a las métricas (tiempo real).
	h.recordIngreso(ctx, email, plan, monto, app, "pago")

	_ = h.Store.EliminarRegistro(ctx, compraID)
	_ = h.Store.RegistrarSuscripcionMeses(ctx, email, plan, monto, 1, app)
	_ = h.FB.ActualizarEstadoCompra(ctx, email, compraID, "pagado", map[string]any{
		"fechaPago":   firebase.Timestamp{Time: time.Now()},
		"mpPaymentId": paymentID, "mpStatus": pago.Status,
	})
}

func orDef(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

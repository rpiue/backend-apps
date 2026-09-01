package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex/backend/internal/cache"
	"codex/backend/internal/chat"
	"codex/backend/internal/firebase"
	"codex/backend/internal/handlers"
	"codex/backend/internal/notify"
)

// Cache en memoria de Plans por app. La disponibilidad de planes (si la app tiene
// planes personales/grupales configurados) casi no cambia, y calcularla implica
// un read completo a Firebase (GetAppData), que es lo que hacía lento abrir la
// ficha del cliente / "Dar plan". Con este TTL corto se sirve al instante.
type plansCacheEntry struct {
	plans []chat.PlanOption
	at    time.Time
}

var (
	plansCacheMu sync.RWMutex
	plansCache   = map[string]plansCacheEntry{}
)

const plansCacheTTL = 5 * time.Minute

// fbDirectory adapta *firebase.Client a chat.UserDirectory. Guarda también el
// *handlers.Handler para reusar sus efectos secundarios (ingreso + acceso cruzado
// Yape↔BCP) al otorgar un plan desde el panel, sin duplicar esa lógica.
type fbDirectory struct {
	fb *firebase.Client
	h  *handlers.Handler
}

func (d fbDirectory) Lookup(ctx context.Context, app, email string, pin *string) ([]chat.FirebaseUser, string, error) {
	p, name := d.fb.Registry.UserDB(app)
	users, err := d.fb.GetUserByEmail(ctx, p, email, pin)
	if err != nil {
		return nil, name, err
	}
	out := make([]chat.FirebaseUser, 0, len(users))
	for _, u := range users {
		fu := chat.FirebaseUser{
			ID:        anyToStr(u["id"]),
			Email:     anyToStr(u["email"]),
			Nombre:    anyToStrPtr(u["nombre"]),
			Apellidos: anyToStrPtr(u["apellidos"]),
			Telefono:  anyToStrPtr(u["telefono"]),
			Clave:     anyToStrPtr(u["clave"]),
			Plan:      anyToStrPtr(u["plan"]),
		}
		// Vencimiento (formateado) y estado de acceso, para que la IA pueda
		// responder sobre la cuenta del PROPIO usuario.
		if t, ok := firebase.AsTime(u["fecha_final"]); ok {
			s := t.Format("2006-01-02")
			fu.FechaFinal = &s
		}
		if v, ok := u["acceso"]; ok {
			acc := "inactivo"
			switch b := v.(type) {
			case bool:
				if b {
					acc = "activo"
				}
			case string:
				if b == "true" || b == "1" {
					acc = "activo"
				}
			}
			fu.Acceso = &acc
		}
		if n, ok := anyToInt64(u["comprasPlan"]); ok {
			fu.Compras = &n
		}
		out = append(out, fu)
	}
	return out, name, nil
}

// GrantPlan otorga un plan desde el panel. Un plan GRUPAL no es solo un nombre en
// el doc del usuario: como en el webhook de pago (procesarPago), pasa por
// GenerarCodigosParaUsuario, que registra al vendedor en controlPagos, le da el
// plan base y crea/renueva sus códigos de activación. Sin eso, el usuario quedaba
// con el plan escrito pero sin códigos que repartir.
func (d fbDirectory) GrantPlan(ctx context.Context, app, email, nombre, plan string) (chat.GrantResult, error) {
	p, _ := d.fb.Registry.UserDB(app)

	var ok bool
	var msg, fecha string
	if opcion, esGrupal := firebase.OpcionDePlanGrupal(plan); esGrupal {
		res, err := d.fb.GenerarCodigosParaUsuario(ctx, p, email, nombre, opcion)
		if err != nil {
			return chat.GrantResult{}, err
		}
		ok, msg, fecha = res.Success, res.Message, fechaISO(res.FechaFinal)
	} else {
		res, err := d.fb.DarPlan(ctx, p, email, plan, firebase.DarPlanOpts{})
		if err != nil {
			return chat.GrantResult{}, err
		}
		ok, msg, fecha = res.Success, res.Message, fechaISO(res.FechaFinal)
	}

	out := chat.GrantResult{OK: ok, Message: msg, FechaFinal: fecha}
	if ok && d.h != nil {
		// Efectos de una compra: registra el ingreso y propaga el acceso cruzado
		// (Yape↔BCP). Los códigos/activaciones vuelven para avisar por chat.
		cr := d.h.GrantSideEffects(ctx, app, email, plan)
		for _, c := range cr.Codes {
			out.Cross = append(out.Cross, chat.CrossGrant{App: c.App, Codigo: c.Codigo, Plan: c.Plan})
		}
		for _, a := range cr.Activated {
			out.Cross = append(out.Cross, chat.CrossGrant{App: a, Plan: plan})
		}
	}
	return out, nil
}

func fechaISO(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (d fbDirectory) GrantDevice(ctx context.Context, app, email, deviceID string) (bool, string, error) {
	p, _ := d.fb.Registry.UserDB(app)
	res, err := d.fb.CambiarMovilBypass(ctx, p, email, deviceID)
	if err != nil {
		return false, "", err
	}
	return res.Success, res.Message, nil
}

// planCatalog son los planes que DarPlan sabe otorgar, agrupados. Solo se
// ofrecen los grupales si la app tiene la colección planGrupal poblada.
var planCatalogPersonal = []chat.PlanOption{
	{Value: "Basico", Label: "Básico", Group: "personal"},
	{Value: "Medium", Label: "Medium", Group: "personal"},
	{Value: "Premium", Label: "Premium", Group: "personal"},
}
var planCatalogGrupal = []chat.PlanOption{
	{Value: "Basico Grupal", Label: "Básico Grupal", Group: "grupal"},
	{Value: "Medium Grupal", Label: "Medium Grupal", Group: "grupal"},
	{Value: "Medium Grupal Plus", Label: "Medium Grupal Plus", Group: "grupal"},
}

func (d fbDirectory) Plans(ctx context.Context, app string) ([]chat.PlanOption, error) {
	plansCacheMu.RLock()
	if e, ok := plansCache[app]; ok && time.Since(e.at) < plansCacheTTL {
		plansCacheMu.RUnlock()
		return e.plans, nil
	}
	plansCacheMu.RUnlock()

	db, _, ok := d.fb.Registry.AppDataDB(app)
	if !ok {
		return nil, nil
	}
	data, err := d.fb.GetAppData(ctx, db, nil, nil, nil)
	if err != nil {
		return nil, err // no cachear errores
	}
	out := make([]chat.PlanOption, 0, len(planCatalogPersonal)+len(planCatalogGrupal))
	if len(data.Planes) > 0 {
		out = append(out, planCatalogPersonal...)
	}
	if len(data.PlanGrupal) > 0 {
		out = append(out, planCatalogGrupal...)
	}
	plansCacheMu.Lock()
	plansCache[app] = plansCacheEntry{plans: out, at: time.Now()}
	plansCacheMu.Unlock()
	return out, nil
}

// notifPush adapta *notify.Notifier a chat.Notifier.
//
// Cada aviso se envía por DOS vías a la vez:
//   - Topic FCM `UserTopic(email, app)`: cubre a los dispositivos suscritos.
//   - Tokens directos guardados en Redis (`fcmtokens:<email>` que llena
//     /register-fcm): más fiable para alcanzar una app CERRADA o matada, y
//     cubre la ventana en que la suscripción al topic aún no propagó en Google.
//
// No genera avisos duplicados: ambos mensajes llevan el mismo `tag`, y Android
// usa ese tag para REEMPLAZAR la notificación en la bandeja (se ve una sola).
// Es la pieza que faltaba para que un INVITADO reciba la respuesta del soporte
// aunque haya cerrado la app y el soporte tarde en contestar.
type notifPush struct {
	n *notify.Notifier
	c *cache.Cache
}

// Push (sin tag) va SOLO por topic: se usa para avisos al admin y, sin un tag
// que permita a Android reemplazar la notificación, sumar el token directo
// mostraría el aviso duplicado. La entrega reforzada por token se reserva a
// PushTagged (avisos al usuario), donde el tag deduplica el display.
func (p notifPush) Push(ctx context.Context, email, app, title, body, route string) error {
	return p.n.NotifyUser(ctx, email, app, notify.Message{
		Title: title, Body: body, Route: route, ChannelID: "alerts", HeadsUp: true,
	})
}

func (p notifPush) PushTagged(ctx context.Context, email, app, title, body, route, tag string) error {
	msg := notify.Message{
		Title: title, Body: body, Route: route, ChannelID: "alerts", HeadsUp: true,
		Tag: tag, CollapseKey: tag,
	}
	err := p.n.NotifyUser(ctx, email, app, msg)
	p.sendToStoredTokens(ctx, email, msg)
	return err
}

// sendToStoredTokens entrega el aviso a cada token FCM que /register-fcm dejó en
// Redis para ese email. Best-effort: los fallos no rompen el envío por topic.
// Un token que FCM reporta como inexistente (404) o inválido (400) se elimina
// del set para que no se acumule basura.
func (p notifPush) sendToStoredTokens(ctx context.Context, email string, msg notify.Message) {
	if p.c == nil || email == "" {
		return
	}
	key := "fcmtokens:" + email
	tokens, err := p.c.SMembers(ctx, key)
	if err != nil || len(tokens) == 0 {
		return
	}
	// Mode "new" para que el cliente lo trate como notificación entrante (igual
	// que el envío por topic, que NotifyUser fija en "new").
	if msg.Mode == "" {
		msg.Mode = "new"
	}
	for _, tk := range tokens {
		if sendErr := p.n.NotifyToken(ctx, tk, msg); sendErr != nil {
			if isDeadTokenErr(sendErr) {
				_ = p.c.SRem(ctx, key, tk)
			}
		}
	}
}

// isDeadTokenErr reconoce el error de FCM para un token ya no válido, que debe
// purgarse. SendToToken devuelve "fcm <status>: <body>".
func isDeadTokenErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.HasPrefix(s, "fcm 404") || strings.HasPrefix(s, "fcm 400") {
		return true
	}
	return strings.Contains(s, "UNREGISTERED") || strings.Contains(s, "INVALID_ARGUMENT")
}

func anyToStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func anyToStrPtr(v any) *string {
	s := anyToStr(v)
	if s == "" {
		return nil
	}
	return &s
}

// anyToInt64 normaliza un número de Firestore (int64/float64/string) a int64.
// El segundo retorno es false si el valor está ausente o no es numérico.
func anyToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		if x, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return x, true
		}
	}
	return 0, false
}

package handlers

import (
	"context"
	"log"
	"strings"

	"codex/backend/internal/firebase"
	"codex/backend/internal/notify"
)

// CrossCode es un código de activación generado para una app destino donde el
// usuario aún no existe. Se devuelve al frontend para mostrarlo directamente.
type CrossCode struct {
	App    string `json:"app"`
	Codigo string `json:"codigo"`
	Plan   string `json:"plan"`
}

// CrossResult resume la propagación cruzada: los códigos generados (apps donde el
// usuario no existía) y las apps donde SÍ existía y se activó el plan directo.
type CrossResult struct {
	Codes     []CrossCode // apps sin cuenta → código a enviar
	Activated []string    // apps con cuenta → ya activadas
}

// GrantSideEffects aplica los efectos de una activación MANUAL desde el panel:
// registra el ingreso (precio estándar del plan) y propaga el acceso a las otras
// apps (Yape↔BCP). Devuelve el resultado cruzado para que el chat avise al cliente.
// El contador de compras se incrementa dentro de DarPlan (en Firebase).
func (h *Handler) GrantSideEffects(ctx context.Context, app, email, plan string) CrossResult {
	// Ingreso al precio REAL del plan (Firebase); 0 deja que recordIngreso caiga al
	// precio estándar solo si Firebase no lo tiene.
	monto, _ := h.precioDesdeFirebase(ctx, app, plan)
	h.recordIngreso(ctx, email, plan, monto, app, "panel")
	return h.crossGrantFull(ctx, app, email, plan, false)
}

// crossGrant propaga el acceso a las demás apps del set CROSS_APPS (Yape↔BCP)
// tras conceder un plan en sourceApp. Para cada app destino distinta:
//   - si el correo YA existe en esa app → DarPlan directo (+ aviso al usuario).
//   - si NO existe → genera un código de 6 chars y lo DEVUELVE. El código no
//     revela la app: Postgres la resuelve al canjearlo en /api/activar.
//
// Si pushCode es true (flujos asíncronos: webhooks de pago, sin frontend que
// muestre el código), además lo envía por notificación. En los flujos con
// respuesta al frontend (negroide, consumirCode) se pasa false y el código va
// en el JSON. Es best-effort: los fallos se registran, no rompen el flujo.
func (h *Handler) crossGrant(ctx context.Context, sourceApp, email, plan string, pushCode bool) []CrossCode {
	return h.crossGrantFull(ctx, sourceApp, email, plan, pushCode).Codes
}

func (h *Handler) crossGrantFull(ctx context.Context, sourceApp, email, plan string, pushCode bool) CrossResult {
	email = strings.ToLower(strings.TrimSpace(email))
	out := CrossResult{Codes: []CrossCode{}, Activated: []string{}}
	if email == "" || plan == "" {
		return out
	}
	_, srcName := h.FB.Registry.UserDB(sourceApp)

	for _, target := range h.Cfg.CrossApps {
		dbT, appName := h.FB.Registry.UserDB(target)
		if appName == srcName {
			continue // no cruzar a la misma app de origen
		}

		disponible, err := h.FB.ExisteUsuario(ctx, dbT, "", email)
		if err != nil {
			log.Printf("[crossGrant] %s existeUsuario: %v", appName, err)
			continue
		}

		if !disponible {
			// El correo ya existe en la app destino → dar el plan directo.
			res, err := h.FB.DarPlan(ctx, dbT, email, plan, firebase.DarPlanOpts{})
			if err != nil {
				log.Printf("[crossGrant] %s darPlan: %v", appName, err)
				continue
			}
			if res.Success {
				out.Activated = append(out.Activated, appName)
				_ = h.Notifier.NotifyUser(ctx, email, appName, notify.Message{
					Title:     "🎉 Acceso activado",
					Body:      "También activamos tu plan " + plan + " en la app " + strings.ToUpper(appName) + ".",
					ChannelID: "alerts",
				})
			}
			continue
		}

		// No existe en la app destino → generar código de activación.
		code, err := h.Store.GenerateActivationCode(ctx, email, appName, plan)
		if err != nil {
			log.Printf("[crossGrant] %s generar código: %v", appName, err)
			continue
		}
		out.Codes = append(out.Codes, CrossCode{App: appName, Codigo: code, Plan: plan})

		if pushCode {
			_ = h.Notifier.NotifyUser(ctx, email, srcName, notify.Message{
				Title:     "Tu código de activación",
				Body:      "Usa el código " + code + " para activar tu plan " + plan + " en la app " + strings.ToUpper(appName) + ".",
				ChannelID: "alerts",
			})
		}
	}
	return out
}

package handlers

import (
	"context"
	"strings"

	"codex/backend/internal/hub"
)

// planMonto: precio estándar de cada plan en soles. Incluye los grupales.
// Se usa cuando se concede acceso sin un monto explícito (p.ej. /negroide).
var planMonto = map[string]int{
	"basico":             30,
	"medium":             35,
	"basico grupal":      50,
	"medium grupal":      85,
	"medium grupal plus": 130,
	"premium":            0,
}

// MontoDePlan devuelve el precio estándar de un plan (0 si no se reconoce).
func MontoDePlan(plan string) int {
	return planMonto[strings.ToLower(strings.TrimSpace(plan))]
}

// recordIngreso registra un ingreso (suma a las métricas) y lo difunde por WS
// para que el panel se actualice en tiempo real. Si monto<=0, usa el precio del plan.
func (h *Handler) recordIngreso(ctx context.Context, email, plan string, monto int, app, fuente string) {
	if monto <= 0 {
		monto = MontoDePlan(plan)
	}
	app = strings.ToLower(strings.TrimSpace(app)) // normaliza para que coincida con el registro de apps
	if app == "" {
		app = "yape"
	}
	ing, err := h.Store.RegistrarIngreso(ctx, email, plan, monto, app, fuente)
	if err != nil {
		return
	}
	h.Hub.Broadcast(hub.Event{Type: "metrica", Data: ing})
}

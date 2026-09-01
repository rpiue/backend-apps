package handlers

import (
	"net/http"

	"codex/backend/internal/hub"
)

// GET /api/adm/apps — lista de apps del CRM.
func (h *Handler) appsList(w http.ResponseWriter, r *http.Request) {
	apps, err := h.Store.ListApps(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, apps)
}

// POST /api/adm/apps { nombre, color } — crea una app.
func (h *Handler) appsCreate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Nombre string `json:"nombre"`
		Color  string `json:"color"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Nombre == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nombre requerido"})
		return
	}
	a, err := h.Store.CreateApp(r.Context(), b.Nombre, b.Color)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.Hub.Broadcast(hub.Event{Type: "app", Data: a})
	writeJSON(w, http.StatusOK, a)
}

// GET /api/adm/revenue?app=&date=YYYY-MM-DD — resumen e ingresos de la vista de pagos.
func (h *Handler) revenueView(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	date := r.URL.Query().Get("date")
	if date == "" {
		date = nowDate()
	}
	rs, err := h.Store.Revenue(r.Context(), app, date)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

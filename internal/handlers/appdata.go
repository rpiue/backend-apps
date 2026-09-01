package handlers

import (
	"context"
	"log"
	"net/http"
	"time"

	"codex/backend/internal/firebase"
	"codex/backend/internal/resources"
)

// (context se usa en la firma de refrescarDatosApp, llamado también por el cron en fase 3)

// POST /api/datosApp — datos de la app (cacheados en Redis, antes era cacheDatosApp{}).
func (h *Handler) datosApp(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	app := b.App
	if app == "" {
		app = "yape"
	}

	_, name, ok := h.FB.Registry.AppDataDB(app)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "App no válida"})
		return
	}
	app = name // clave de cache normalizada (consistente con cron e invalidación)

	ctx := r.Context()
	cacheK := "datosApp:" + app

	// 1) cache
	var cached firebase.AppData
	if found, _ := h.Cache.GetJSON(ctx, cacheK, &cached); found {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	// 2) refrescar desde Firebase
	datos, err := h.refrescarDatosApp(ctx, app)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, datos)
}

// refrescarDatosApp consulta Firebase y guarda en cache. Para "yape" usa los
// banners/anuncios en memoria (resources); para "interbank" los toma de Firestore.
func (h *Handler) refrescarDatosApp(ctx context.Context, app string) (firebase.AppData, error) {
	db, name, _ := h.FB.Registry.AppDataDB(app)

	var extraAnuncios, extraBanners []map[string]any
	if name == "yape" {
		extraAnuncios = h.Resources.Anuncios()
		extraBanners = h.Resources.Banners()
	}

	datos, err := h.FB.GetAppData(ctx, db, extraAnuncios, extraBanners, resources.AppsData())
	if err != nil {
		return firebase.AppData{}, err
	}
	// TTL de 6h; el cron de fase 3 lo refrescará proactivamente.
	_ = h.Cache.SetJSON(ctx, "datosApp:"+app, datos, 6*time.Hour)
	log.Printf("[datosApp] refrescado desde Firebase para %s", app)
	return datos, nil
}

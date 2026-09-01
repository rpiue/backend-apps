package handlers

import (
	"net/http"
	"strings"
	"time"
)

// coleccionesEditables son las colecciones Firestore que el panel puede editar
// (las mismas que alimentan /datosApp). Allowlist por seguridad.
var coleccionesEditables = map[string]bool{
	"planes": true, "planGrupal": true, "anuncios": true, "banners": true,
}

// GET /api/adm/app-config?app=yape — devuelve TODO lo editable del app (lo mismo
// que ve la app en /datosApp): datos/app + planes + planGrupal + anuncios + banners.
func (h *Handler) appConfigGet(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	db, name, ok := h.FB.Registry.AppDataDB(app)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "app desconocida"})
		return
	}
	data, err := h.FB.GetAppData(r.Context(), db, nil, nil, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// btcDisponible: estado efectivo del pago BTC para esta app. serverBtc indica
	// si el servidor tiene BTCPay configurado (sin eso, el toggle no cobra nada).
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "app": name,
		"datosApp": data.DatosApp, "planes": data.Planes, "planGrupal": data.PlanGrupal,
		"anuncios": data.Anuncios, "banners": data.Banners,
		"btcDisponible": h.btcDisponibleParaApp(r.Context(), name),
		"serverBtc":     h.BTC != nil && h.BTC.Enabled(),
	})
}

// POST /api/adm/app-config/btc — activa o desactiva el pago por BTC de una app.
// Body: { app, enabled }. Escribe el flag `btcDisponible` en datos/app; la app
// móvil lo consulta vía GET /api/sara.
func (h *Handler) appConfigBTC(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App     string `json:"app"`
		Enabled bool   `json:"enabled"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	db, name, ok := h.FB.Registry.AppDataDB(b.App)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "app desconocida"})
		return
	}
	if err := h.FB.UpdateFields(r.Context(), db, "datos/app", map[string]any{
		"btcDisponible": b.Enabled,
		"actualizado":   time.Now().UnixMilli(),
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.invalidarDatosApp(r, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "app": name, "btcDisponible": b.Enabled,
		"serverBtc": h.BTC != nil && h.BTC.Enabled(),
	})
}

// POST /api/adm/app-config/datos — actualiza campos del doc datos/app
// (version, mantenimiento, nombre, etc.). Body: { app, datos: {...} }.
func (h *Handler) appConfigDatos(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App   string         `json:"app"`
		Datos map[string]any `json:"datos"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if len(b.Datos) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "datos vacío"})
		return
	}
	db, name, ok := h.FB.Registry.AppDataDB(b.App)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "app desconocida"})
		return
	}
	// Marca de actualización (como el JS).
	b.Datos["actualizado"] = time.Now().UnixMilli()
	if err := h.FB.UpdateFields(r.Context(), db, "datos/app", b.Datos); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.invalidarDatosApp(r, name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "app": name, "datos": b.Datos})
}

// POST /api/adm/app-config/coleccion — crea/actualiza un doc de una colección
// editable. Body: { app, coleccion, id?, data }. Sin id => crea (AddDoc).
func (h *Handler) appConfigColeccionUpsert(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App       string         `json:"app"`
		Coleccion string         `json:"coleccion"`
		ID        string         `json:"id"`
		Data      map[string]any `json:"data"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if !coleccionesEditables[b.Coleccion] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "colección no editable"})
		return
	}
	if len(b.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "data vacío"})
		return
	}
	db, name, ok := h.FB.Registry.AppDataDB(b.App)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "app desconocida"})
		return
	}
	delete(b.Data, "id") // el id no es un campo del doc
	id := strings.TrimSpace(b.ID)
	if id != "" {
		if err := h.FB.SetDoc(r.Context(), db, b.Coleccion+"/"+id, b.Data); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	} else {
		nuevo, err := h.FB.AddDoc(r.Context(), db, b.Coleccion, b.Data)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		id = nuevo
	}
	h.invalidarDatosApp(r, name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "coleccion": b.Coleccion})
}

// POST /api/adm/app-config/coleccion/eliminar — elimina un doc. Body: { app, coleccion, id }.
func (h *Handler) appConfigColeccionEliminar(w http.ResponseWriter, r *http.Request) {
	var b struct {
		App       string `json:"app"`
		Coleccion string `json:"coleccion"`
		ID        string `json:"id"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if !coleccionesEditables[b.Coleccion] || strings.TrimSpace(b.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "colección o id inválido"})
		return
	}
	db, name, ok := h.FB.Registry.AppDataDB(b.App)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "app desconocida"})
		return
	}
	if err := h.FB.DeleteDoc(r.Context(), db, b.Coleccion+"/"+b.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.invalidarDatosApp(r, name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// invalidarDatosApp borra la cache de /datosApp para que los cambios del panel se
// reflejen de inmediato en la app móvil (en vez de esperar el cron de 6h).
func (h *Handler) invalidarDatosApp(r *http.Request, app string) {
	_ = h.Cache.Del(r.Context(), "datosApp:"+strings.ToLower(app))
}

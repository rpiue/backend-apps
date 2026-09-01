package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// MobileRouter monta las rutas que consume la app móvil bajo /api.
// IMPORTANTE: las rutas y sus respuestas deben coincidir con el index.js original
// para no tener que tocar la app móvil.
func (h *Handler) MobileRouter() http.Handler {
	r := chi.NewRouter()

	cfg := h.Cfg

	// --- Rutas públicas ---
	r.Get("/version", h.version)
	r.Get("/hora-peru", h.horaPeru)
	r.Get("/acceso/pubkey", h.accesoPubkey)
	r.Get("/descargar", h.descargar)
	r.With(h.rl("index", cfg.RLLoginPerMin)).Post("/index", h.authIndex)
	r.With(h.rl("login", cfg.RLLoginPerMin)).Post("/login", h.login)
	r.Post("/datosApp", h.datosApp)
	r.Post("/verificar-disponible", h.verificarDisponible)
	r.Post("/updateU", h.updateUser)
	r.With(h.rl("crear", cfg.RLCreatePerMin)).Post("/crearUsuario", h.crearUsuario)

	// --- Rutas protegidas con token (verifyToken del JS) ---
	r.Group(func(pr chi.Router) {
		pr.Use(h.Auth.Middleware)
		pr.Post("/datosUsuario", h.datosUsuario)
		pr.Post("/acceso", h.acceso)
		pr.Post("/update", h.updateUser)
		pr.Post("/updateT", h.updateT)
		pr.Post("/delate", h.delate)
		pr.Post("/cambiar", h.cambiar)
		pr.Post("/negroide", h.negroide)
		pr.Post("/telefono", h.telefono)
		pr.Post("/misCodes", h.misCodes)
		pr.With(h.rl("consumir", cfg.RLCodePerMin)).Post("/consumirCode", h.consumirCode)
	})

	// --- Pagos y códigos (públicas) ---
	r.With(h.rl("verificar", cfg.RLCodePerMin)).Post("/verificarCode", h.verificarCode)
	r.With(h.rl("activar", cfg.RLCodePerMin)).Post("/activar", h.activarCode)
	r.Get("/sara", h.saraConfig) // consulta de métodos disponibles (BTC on/off por app)
	r.With(h.rl("sara", cfg.RLSaraPerMin)).Post("/sara", h.sara)
	r.Post("/webhook", h.webhook)
	r.Post("/webhook/btcpay", h.webhookBTCPay)

	return r
}

// version replica GET /api/version del server actual: health-check que devuelve 204.
func (h *Handler) version(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// apkURLs replica APK_URLS del index.js (descarga de APK por app/abi).
var apkURLs = map[string]map[string]string{
	"bcp": {
		"arm64-v8a":   "https://codexpe.com/apps/bcp/app-arm64-v8a.apk",
		"armeabi-v7a": "https://codexpe.com/apps/bcp/app-armeabi-v7a.apk",
		"x86":         "https://codexpe.com/apps/bcp/app-x86.apk",
		"x86_64":      "https://codexpe.com/apps/bcp/app-x86_64.apk",
	},
	"yape": {
		"arm64-v8a":   "https://codexpe.com/apps/app-arm64-v8a.apk",
		"armeabi-v7a": "https://codexpe.com/apps/app-armeabi-v7a.apk",
		"x86":         "https://codexpe.com/apps/app-x86.apk",
		"x86_64":      "https://codexpe.com/apps/app-x86_64.apk",
	},
}

func normalizeAbi(abi string) string {
	a := strings.ToLower(abi)
	switch {
	case strings.Contains(a, "arm64") || strings.Contains(a, "v8a"):
		return "arm64-v8a"
	case strings.Contains(a, "armeabi") || strings.Contains(a, "v7a"):
		return "armeabi-v7a"
	case strings.Contains(a, "x86_64"):
		return "x86_64"
	case strings.Contains(a, "x86"):
		return "x86"
	}
	return "arm64-v8a"
}

// GET /api/descargar?app=&abi= → URL del APK.
func (h *Handler) descargar(w http.ResponseWriter, r *http.Request) {
	app := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("app")))
	abi := normalizeAbi(r.URL.Query().Get("abi"))
	if app == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Missing query param: app"})
		return
	}
	appMap, ok := apkURLs[app]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Unknown app: " + app})
		return
	}
	url := appMap[abi]
	if url == "" {
		url = appMap["arm64-v8a"]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "app": app, "abi": abi, "url": url})
}

// horaPeru replica GET /api/hora-peru: { hora: "YYYY-MM-DD HH:mm:ss", zona: "America/Lima" }.
func (h *Handler) horaPeru(w http.ResponseWriter, _ *http.Request) {
	loc, err := time.LoadLocation("America/Lima")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "zona horaria no disponible"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"hora": time.Now().In(loc).Format("2006-01-02 15:04:05"),
		"zona": "America/Lima",
	})
}

package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// AdminRouter monta /api/adm (panel). /login es público; el resto requiere sesión.
func (h *Handler) AdminRouter() http.Handler {
	r := chi.NewRouter()

	// Público
	r.Post("/login", h.adminLogin)

	// Protegido (JWT de sesión o x-api-key)
	r.Group(func(pr chi.Router) {
		pr.Use(h.adminAuth)

		pr.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
		pr.Get("/me", h.adminMe)
		pr.Get("/stats", h.adminStats)

		// Apps (multi-app) + vista de ingresos
		pr.Get("/apps", h.appsList)
		pr.Post("/apps", h.appsCreate)
		pr.Get("/revenue", h.revenueView)

		// CRM: ficha de usuario + métricas de altas
		pr.Get("/usuario", h.usuarioCRM)
		pr.Get("/usuarios-metrica", h.usuariosMetrica)

		// Cuenta: cambiar contraseña
		pr.Post("/password", h.changePassword)

		// Respuestas rápidas (configurables)
		pr.Get("/respuestas", h.respuestasList)
		pr.Post("/respuestas", h.respuestaCreate)
		pr.Post("/respuestas/eliminar", h.respuestaDelete)

		// Configuración del app (Firestore): version/mantenimiento + planes/anuncios/banners.
		pr.Get("/app-config", h.appConfigGet)
		pr.Post("/app-config/datos", h.appConfigDatos)
		pr.Post("/app-config/coleccion", h.appConfigColeccionUpsert)
		pr.Post("/app-config/coleccion/eliminar", h.appConfigColeccionEliminar)
		pr.Post("/app-config/btc", h.appConfigBTC)

		// Token de chat-admin para el panel (entra a /api/chat sin 2º login).
		pr.Get("/chat-token", h.chatToken)

		// Chat estilo WhatsApp (legacy simple; el avanzado vive en /api/chat)
		pr.Get("/chat/conversaciones", h.chatConversaciones)
		pr.Get("/chat/mensajes", h.chatMensajes)
		pr.Post("/chat/enviar", h.chatEnviar)
		pr.Post("/chat/leidos", h.chatLeidos)
		pr.Post("/chat/incoming", h.chatIncoming)

		// IA (Ollama)
		pr.Get("/ai/config", h.aiConfigGet)
		pr.Post("/ai/config", h.aiConfigSet)
		pr.Post("/ai/reload", h.aiReload)

		// Subida de imágenes
		pr.Post("/upload", h.adminUpload)
	})

	// WebSocket en tiempo real (auth por ?token= o ?key=, dentro del propio handler).
	r.Get("/ws", h.wsAuth)

	return r
}

// wsAuth valida el WS por query (?token= JWT o ?key= api-key) y abre el socket.
func (h *Handler) wsAuth(w http.ResponseWriter, r *http.Request) {
	ok := false
	if k := r.URL.Query().Get("key"); k != "" && k == h.Cfg.AdminAPIKey {
		ok = true
	}
	if !ok {
		if tok := r.URL.Query().Get("token"); tok != "" {
			if claims, err := h.Auth.Verify(tok); err == nil {
				if adm, _ := claims["adm"].(bool); adm {
					ok = true
				}
			}
		}
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No autorizado."})
		return
	}
	h.Hub.Handle(w, r)
}

func (h *Handler) adminStats(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	a, _ := h.Store.GetAnalytics(r.Context(), app)
	writeJSON(w, http.StatusOK, a)
}

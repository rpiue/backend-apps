package handlers

import (
	"net/http"
	"strings"
)

// adminEmailFromReq extrae el email del admin del token de sesión.
func (h *Handler) adminEmailFromReq(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if claims, err := h.Auth.Verify(strings.TrimSpace(tok)); err == nil {
		if e, ok := claims["email"].(string); ok {
			return e
		}
	}
	return ""
}

// POST /api/adm/password { actual, nueva } — cambia la contraseña del admin.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Actual string `json:"actual"`
		Nueva  string `json:"nueva"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	email := h.adminEmailFromReq(r)
	if email == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sesión inválida"})
		return
	}
	if err := h.Store.ChangePassword(r.Context(), email, b.Actual, b.Nueva); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Respuestas rápidas (configurables para el chat) ---

func (h *Handler) respuestasList(w http.ResponseWriter, r *http.Request) {
	rs, err := h.Store.ListRespuestas(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (h *Handler) respuestaCreate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Texto string `json:"texto"`
		Orden int    `json:"orden"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Texto) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "texto requerido"})
		return
	}
	rr, err := h.Store.CreateRespuesta(r.Context(), strings.TrimSpace(b.Texto), b.Orden)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

func (h *Handler) respuestaDelete(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if err := h.Store.DeleteRespuesta(r.Context(), b.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/adm/usuarios-metrica?app=&desde=&hasta= — altas de usuarios con filtros.
func (h *Handler) usuariosMetrica(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m, err := h.Store.GetUsuariosMetrics(r.Context(), q.Get("app"), q.Get("desde"), q.Get("hasta"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// GET /api/adm/usuario?email=&app= — ficha CRM de un usuario (Firebase).
func (h *Handler) usuarioCRM(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email requerido"})
		return
	}
	db, _ := h.FB.Registry.UserDB(r.URL.Query().Get("app"))
	ficha, err := h.FB.UsuarioCRM(r.Context(), db, email)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ficha)
}

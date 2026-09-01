package handlers

import (
	"net/http"
	"strings"
	"time"

	secmw "codex/backend/internal/middleware"
)

// adminLogin: POST /api/adm/login { email, password } → { token, user }.
func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	u, err := h.Store.CheckLogin(r.Context(), b.Email, b.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Email o contraseña incorrectos"})
		return
	}
	token, err := h.Auth.Sign(map[string]any{"adm": true, "email": u.Email}, 12*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no se pudo emitir el token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user":  map[string]string{"email": u.Email, "nombre": u.Nombre, "rol": u.Rol},
	})
}

// adminAuth protege /api/adm: acepta JWT de sesión (Authorization o ?token=)
// y, como compatibilidad, también la x-api-key.
func (h *Handler) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1) x-api-key (compatibilidad / integraciones). La comparación es en
		// tiempo constante: con `==` el tiempo de respuesta revela cuántos bytes
		// iniciales acertó quien prueba, y la clave se reconstruye midiendo.
		if k := r.Header.Get("x-api-key"); secmw.SecretoIgual(k, h.Cfg.AdminAPIKey) {
			next.ServeHTTP(w, r)
			return
		}
		// 2) JWT de sesión.
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok = strings.TrimSpace(tok)
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok != "" {
			if claims, err := h.Auth.Verify(tok); err == nil {
				if adm, _ := claims["adm"].(bool); adm {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No autorizado."})
	})
}

// adminMe: datos del admin logueado (para el perfil).
func (h *Handler) adminMe(w http.ResponseWriter, r *http.Request) {
	email := ""
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if claims, err := h.Auth.Verify(strings.TrimSpace(tok)); err == nil {
		email, _ = claims["email"].(string)
	}
	if email == "" {
		writeJSON(w, http.StatusOK, map[string]string{"email": "", "nombre": "Admin", "rol": "admin"})
		return
	}
	u, err := h.Store.GetAdmin(r.Context(), email)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"email": email, "nombre": "Admin", "rol": "admin"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": u.Email, "nombre": u.Nombre, "rol": u.Rol})
}

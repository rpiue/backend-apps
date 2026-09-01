// Package auth replica la emisión/validación de JWT que hacía index.js con
// la librería jsonwebtoken (HS256). El token expira en 65s, igual que el original.
package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type ctxKey string

const userKey ctxKey = "user"

type Auth struct {
	secret []byte
}

func New(secret string) *Auth { return &Auth{secret: []byte(secret)} }

// Sign emite un token con los claims indicados y expiración en ttl.
// Mirrors jwt.sign({...}, secret, { expiresIn: '65s' }).
func (a *Auth) Sign(claims map[string]any, ttl time.Duration) (string, error) {
	mc := jwt.MapClaims{}
	for k, v := range claims {
		mc[k] = v
	}
	now := time.Now()
	mc["iat"] = now.Unix()
	mc["exp"] = now.Add(ttl).Unix()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, mc)
	return tok.SignedString(a.secret)
}

// Verify valida el token y devuelve los claims.
func (a *Auth) Verify(token string) (jwt.MapClaims, error) {
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimSpace(token)
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return a.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// Middleware replica verifyToken de index.js: lee el header Authorization
// (sin prefijo Bearer obligatorio) y responde 401 si el token falta o es inválido.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No token provided"})
			return
		}
		claims, err := a.Verify(token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid token"})
			return
		}
		ctx := context.WithValue(r.Context(), userKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ClaimsFromContext devuelve los claims del JWT que Middleware validó y guardó
// en el contexto. ok=false si no hubo un token válido en la petición.
func ClaimsFromContext(ctx context.Context) (jwt.MapClaims, bool) {
	c, ok := ctx.Value(userKey).(jwt.MapClaims)
	return c, ok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = encodeJSON(w, v)
}

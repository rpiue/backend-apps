package handlers

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex/backend/internal/auth"
	"codex/backend/internal/config"
	"codex/backend/internal/firebase"
)

// accessSeedFile es el nombre del archivo donde se persiste la semilla Ed25519
// dentro del directorio de secretos, cuando no se configuró ACCESS_SIGNING_KEY.
const accessSeedFile = "access_signing.key"

// --- Firmador de acceso (Ed25519) ---------------------------------------------
//
// Emite tokens firmados que certifican, con la HORA DEL SERVIDOR, hasta cuándo
// un usuario tiene acceso. La app móvil embebe la clave pública y verifica la
// firma offline: así, aunque alguien parchee el APK o cambie la fecha del
// dispositivo, no puede fabricar un acceso válido sin la clave privada (que solo
// vive en el servidor). Es aditivo: los APK viejos siguen usando /datosUsuario.

var (
	accessSignerOnce sync.Once
	accessPriv       ed25519.PrivateKey
	accessPub        ed25519.PublicKey
)

// initAccessSigner resuelve la clave de firma Ed25519 con esta prioridad:
//
//  1. ACCESS_SIGNING_KEY (env): semilla de 32 bytes en base64. Máxima prioridad
//     para permitir rotación/control explícito en producción.
//  2. Archivo persistido <secretsDir>/access_signing.key: si existe y es válido,
//     se reutiliza, de modo que la clave pública NO cambia entre reinicios (la
//     app embebe la pública y debe seguir validando).
//  3. Si no hay ninguna, se genera una nueva y se PERSISTE en ese archivo. Así
//     el primer arranque fija la clave de forma estable, sin operación manual.
//
// El fallback anterior (clave efímera en memoria) cambiaba en cada reinicio y
// rompía la verificación offline; por eso ahora se persiste.
func (h *Handler) initAccessSigner() {
	accessSignerOnce.Do(func() {
		// (1) Variable de entorno.
		envSeed := ""
		if h.Cfg != nil {
			envSeed = strings.TrimSpace(h.Cfg.AccessSigningKey)
		}
		if envSeed != "" {
			if priv, ok := privFromSeedB64(envSeed); ok {
				accessPriv = priv
				accessPub = priv.Public().(ed25519.PublicKey)
				return
			}
			log.Printf("[acceso] ACCESS_SIGNING_KEY inválida; se intenta clave persistida")
		}

		// (2) Archivo persistido en el directorio de secretos.
		seedPath := accessSeedPath(h.Cfg)
		if data, err := os.ReadFile(seedPath); err == nil {
			if priv, ok := privFromSeedB64(strings.TrimSpace(string(data))); ok {
				accessPriv = priv
				accessPub = priv.Public().(ed25519.PublicKey)
				return
			}
			log.Printf("[acceso] archivo de semilla %q inválido; se regenerará", seedPath)
		}

		// (3) Generar y persistir una nueva.
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		accessPriv, accessPub = priv, pub
		seedB64 := base64.StdEncoding.EncodeToString(priv.Seed())
		if err := persistAccessSeed(seedPath, seedB64); err != nil {
			log.Printf("[acceso] ADVERTENCIA: no se pudo persistir la semilla en %q (%v); "+
				"la clave será EFÍMERA hasta el próximo arranque", seedPath, err)
		} else {
			log.Printf("[acceso] clave Ed25519 generada y persistida en %q", seedPath)
		}
		log.Printf("[acceso] Clave pública (para embeber en la app): %s",
			base64.StdEncoding.EncodeToString(accessPub))
	})
}

// privFromSeedB64 decodifica una semilla base64 de 32 bytes a una clave privada
// Ed25519. Devuelve ok=false si el formato o la longitud no son válidos.
func privFromSeedB64(seedB64 string) (ed25519.PrivateKey, bool) {
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false
	}
	return ed25519.NewKeyFromSeed(seed), true
}

// accessSeedPath construye la ruta del archivo de semilla dentro del directorio
// de secretos (mismo que las credenciales de Firebase). Default: ./secrets.
func accessSeedPath(cfg *config.Config) string {
	dir := "./secrets"
	if cfg != nil && strings.TrimSpace(cfg.FirebaseCredsDir) != "" {
		dir = cfg.FirebaseCredsDir
	}
	return filepath.Join(dir, accessSeedFile)
}

// persistAccessSeed escribe la semilla base64 con permisos restrictivos (0600),
// creando el directorio si hace falta.
func persistAccessSeed(path, seedB64 string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(seedB64+"\n"), 0o600)
}

// accesoToken es el contenido firmado. Los campos son epoch en segundos.
type accesoToken struct {
	UserID      string `json:"user_id"`
	Valid       bool   `json:"valid"`
	AccessUntil int64  `json:"access_until"` // fecha_final (server-parsed); 0 si no hay
	IssuedAt    int64  `json:"issued_at"`    // hora del servidor al emitir
	ServerNow   int64  `json:"server_now"`   // hora del servidor (para sincronizar reloj)
}

// canonical construye la cadena determinista que se firma/verifica. El cliente
// debe reconstruirla EXACTAMENTE igual. Versionada por si evoluciona el formato.
func (t accesoToken) canonical() string {
	return strings.Join([]string{
		"v1",
		t.UserID,
		strconv.FormatBool(t.Valid),
		strconv.FormatInt(t.AccessUntil, 10),
		strconv.FormatInt(t.IssuedAt, 10),
		strconv.FormatInt(t.ServerNow, 10),
	}, "|")
}

// anyToStr convierte un valor de Firestore a string (vacío si es nil).
func anyToStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// asBool interpreta acceso como bool aunque venga como string ("true"/"1").
func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "true" || s == "1" || s == "si" || s == "sí"
	default:
		return false
	}
}

// POST /api/acceso  (protegido por token)  body: { userID, app }
// Responde { token, sig, alg, pubkey } donde sig = Ed25519(canonical(token)).
func (h *Handler) acceso(w http.ResponseWriter, r *http.Request) {
	h.initAccessSigner()

	var b struct {
		UserID string `json:"userID"`
		App    string `json:"app"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.UserID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "userID requerido"})
		return
	}

	// Anti-IDOR (best-effort): si el JWT trae el claim `uid`, el acceso solicitado
	// debe ser el del propio dueño del token. Los tokens emitidos por cache-hit en
	// /index no llevan `uid`; en ese caso no se exige, para no romper ese flujo ni
	// a otras apps que autentican distinto.
	if claims, ok := auth.ClaimsFromContext(r.Context()); ok {
		if uid, _ := claims["uid"].(string); strings.TrimSpace(uid) != "" && uid != b.UserID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "userID no coincide con el token"})
			return
		}
	}

	ctx := r.Context()
	db, _ := h.FB.Registry.UserDB(b.App)
	data, err := h.FB.GetUserData(ctx, db, b.UserID)
	if err != nil || data == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "usuario no encontrado"})
		return
	}

	now := time.Now()
	var accessUntil int64
	fechaOK := false
	if ff, ok := firebase.AsTime(data["fecha_final"]); ok {
		accessUntil = ff.Unix()
		fechaOK = ff.After(now)
	}
	// El token certifica EXACTAMENTE lo mismo que la app valida localmente
	// (usuarios.dart → hasActiveAccess), para que el token firmado y el chequeo
	// offline nunca se contradigan: plan vigente + fecha_final futura (según la
	// hora del SERVIDOR) + entitlement (QR o crear_contacto). NO se usa el flag
	// `acceso`, que el gate de acceso de la app no considera (antes se exigía y
	// bloqueaba a usuarios con plan válido pero acceso=false).
	plan := strings.ToLower(strings.TrimSpace(anyToStr(data["plan"])))
	planOK := plan != "" && plan != "sin plan" && plan != "<nil>" && plan != "null"
	entitlement := asBool(data["QR"]) || asBool(data["crear_contacto"])
	valid := planOK && fechaOK && entitlement

	tok := accesoToken{
		UserID:      b.UserID,
		Valid:       valid,
		AccessUntil: accessUntil,
		IssuedAt:    now.Unix(),
		ServerNow:   now.Unix(),
	}
	sig := ed25519.Sign(accessPriv, []byte(tok.canonical()))

	writeJSON(w, http.StatusOK, map[string]any{
		"token":  tok,
		"sig":    base64.StdEncoding.EncodeToString(sig),
		"alg":    "Ed25519",
		"pubkey": base64.StdEncoding.EncodeToString(accessPub),
	})
}

// GET /api/acceso/pubkey  (público) → clave pública actual, para diagnóstico y
// para que el build de la app la pueda obtener/verificar.
func (h *Handler) accesoPubkey(w http.ResponseWriter, _ *http.Request) {
	h.initAccessSigner()
	writeJSON(w, http.StatusOK, map[string]string{
		"alg":    "Ed25519",
		"pubkey": base64.StdEncoding.EncodeToString(accessPub),
	})
}

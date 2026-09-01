package chat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"codex/backend/internal/ai"
	"codex/backend/internal/cache"
	"codex/backend/internal/middleware"
)

// FirebaseUser es el usuario que devuelve el directorio (Firebase) para el chat.
type FirebaseUser struct {
	ID        string
	Email     string
	Nombre    *string
	Apellidos *string
	Telefono  *string
	Clave     *string
	// Datos de cuenta (para que la IA pueda responder SOLO sobre el propio usuario).
	Plan       *string
	FechaFinal *string // vencimiento (formateado)
	Acceso     *string // "activo" | "inactivo"
	Compras    *int64  // veces que ha comprado un plan (comprasPlan en Firebase)
}

// CrossGrant informa lo que pasó en OTRA app (Yape↔BCP) al activar un plan:
// si el usuario ya tenía cuenta allí se activó directo (Codigo vacío); si no,
// se generó un código de 6 chars para que la active al descargarla.
type CrossGrant struct {
	App    string // app destino (p.ej. "bcp")
	Codigo string // código de activación; vacío si ya se activó directamente
	Plan   string
}

// GrantResult es el resultado de otorgar un plan desde el panel, con lo necesario
// para que el chat avise al cliente por mensaje automático.
type GrantResult struct {
	OK         bool
	Message    string
	FechaFinal string
	Cross      []CrossGrant // efecto en las otras apps (códigos o activaciones)
}

// UserDirectory abstrae las consultas a Firebase que necesita el chat
// (getUserByEmail, darPlan, cambiarMovil). El adaptador vive en el paquete que
// tiene acceso a *firebase.Client, evitando acoplar este paquete.
type UserDirectory interface {
	// Lookup busca usuarios por email en una app; si pin != nil, filtra por clave.
	// Devuelve los usuarios y el nombre normalizado de la app.
	Lookup(ctx context.Context, app, email string, pin *string) ([]FirebaseUser, string, error)
	// GrantPlan activa un plan (darPlan; los grupales además generan los códigos
	// del vendedor, de ahí `nombre`). Registra el ingreso y propaga el acceso
	// cruzado a las otras apps; el resultado incluye qué mostrar/enviar al cliente.
	GrantPlan(ctx context.Context, app, email, nombre, plan string) (GrantResult, error)
	// GrantDevice cambia el dispositivo activo saltando el cooldown (bypass).
	GrantDevice(ctx context.Context, app, email, deviceID string) (bool, string, error)
	// Plans devuelve los planes otorgables según lo que la app tenga configurado
	// (planes personales y/o grupales). Se usa para poblar el menú del admin.
	Plans(ctx context.Context, app string) ([]PlanOption, error)
}

// Notifier abstrae el envío de notificaciones push (enviarNotificacion / topic).
type Notifier interface {
	Push(ctx context.Context, email, app, title, body, route string) error
	// PushTagged envía con un tag/collapseKey por conversación: las
	// notificaciones con el mismo tag se REEMPLAZAN en el dispositivo (útil para
	// "Tienes N mensajes nuevos", que se actualiza en vez de apilarse).
	PushTagged(ctx context.Context, email, app, title, body, route, tag string) error
}

// Config agrupa la configuración del chat (mismos defaults que index.js).
type Config struct {
	JWTSecret             string
	UploadDir             string
	PaymentQRPath         string
	AdminNotificationMail string // fallback (holaperu1234@gmail.com)
	AdminEmail            string // para el seed del admin
	AdminPassword         string
	MsgRatePerMin         int      // límite duro de mensajes por conversación/min (anti-spam)
	ClamAVAddr            string   // "host:port" o "unix:/ruta.sock"; vacío = escaneo deshabilitado
	AppDownloadURL        string   // web de descarga de las apps (para el mensaje del código cruzado)
	AllowedOrigins        []string // orígenes de navegador que pueden abrir el WebSocket del chat
	TrustedProxies        []string // CIDRs de los que SÍ se acepta X-Forwarded-For (si no, se ignora)
}

func (c *Config) withDefaults() {
	if c.JWTSecret == "" {
		c.JWTSecret = "chat_dev_secret_change_me"
	}
	if c.UploadDir == "" {
		c.UploadDir = filepath.Join(".", "chat-uploads")
	}
	if c.PaymentQRPath == "" {
		c.PaymentQRPath = filepath.Join(c.UploadDir, "qr-de-pago", "qr.jpeg")
	}
	if c.AdminNotificationMail == "" {
		c.AdminNotificationMail = "holaperu1234@gmail.com"
	}
	if c.MsgRatePerMin <= 0 {
		c.MsgRatePerMin = 20
	}
	if c.AppDownloadURL == "" {
		c.AppDownloadURL = "codexpe.com"
	}
}

// Service agrupa todas las dependencias del chat y expone el Router.
type Service struct {
	db    *DB
	rt    *Realtime
	cfg   Config
	dir   UserDirectory
	push  Notifier
	lim   *middleware.Limiter
	cache *cache.Cache // Redis: config de la IA (ai:enabled/prompt/model)

	pendingOffline   map[int64]*time.Timer
	pendingOfflineMu sync.Mutex

	// IA: respondedor con batching. aiBuf junta los mensajes del usuario mientras
	// escribe; aiDeb reinicia el timer de 5s en cada mensaje/typing.
	ai      *ai.Client
	aiDeb   *ai.Debouncer
	aiBuf   map[int64][]string
	aiBufMu sync.Mutex
}

// New crea el servicio de chat, asegura el esquema y siembra admin/automatización.
// c (Redis) es opcional pero recomendado: cachea las consultas pesadas del chat.
func New(ctx context.Context, pool *pgxpool.Pool, c *cache.Cache, cfg Config, dir UserDirectory, push Notifier) (*Service, error) {
	cfg.withDefaults()
	if err := EnsureSchema(ctx, pool, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.UploadDir, 0o755); err != nil {
		return nil, err
	}
	db := newDB(pool, c)
	// El limitador se construye primero: lo comparten el servicio y el módulo de
	// tiempo real, y es quien sabe de qué proxies se acepta la cabecera de IP.
	lim := middleware.NewLimiter(c, cfg.TrustedProxies)
	rt := newRealtime(db, cfg.AllowedOrigins, lim)
	s := &Service{
		db: db, rt: rt, cfg: cfg, dir: dir, push: push, cache: c,
		lim:            lim,
		pendingOffline: map[int64]*time.Timer{},
	}
	rt.setUserConnectedHandler(func(conversationID int64) {
		s.cancelPendingOfflineUserNotification(conversationID)
		// Al reconectarse el usuario, limpia el dedupe para poder volver a
		// notificarlo en una próxima ausencia sin esperar el cooldown.
		_ = s.db.clearNotificationDedupeKey(context.Background(), offlineUserDedupeKey(conversationID))
	})
	// Programador de recordatorios: envía las notificaciones push programadas.
	s.startReminderScheduler(ctx)
	return s, nil
}

// IssueAdminToken emite un token de chat-admin para el admin del chat sin pedir
// contraseña. Se usa desde el panel React (ya autenticado por su propio login)
// para que entre al chat sin un segundo login. Devuelve token + datos del admin.
func (s *Service) IssueAdminToken(r *http.Request) (string, *PublicUser, error) {
	admin, err := s.db.getAdminUser(r.Context())
	if err != nil {
		return "", nil, err
	}
	if admin == nil {
		return "", nil, errNoChatAdmin
	}
	token, err := s.signSessionFor(s.fingerprintFromRequest(r), s.clientIP(r), admin.ID, "admin", admin.App, nil, admin.Email, nil)
	if err != nil {
		return "", nil, err
	}
	return token, admin, nil
}

var errNoChatAdmin = &chatError{"admin de chat no inicializado"}

type chatError struct{ s string }

func (e *chatError) Error() string { return e.s }

// --- JWT (CHAT_JWT_SECRET) ---

type chatClaims struct {
	Sub            int64  `json:"sub"`
	Role           string `json:"role"`
	App            string `json:"app"`
	ConversationID *int64 `json:"conversationId"`
	Email          string `json:"email"`
	JTI            string `json:"jti"`
	jwt.RegisteredClaims
}

func (s *Service) signToken(userID int64, role, app string, conversationID *int64, email, jti string) (string, error) {
	claims := chatClaims{
		Sub: userID, Role: role, App: app, ConversationID: conversationID, Email: email, JTI: jti,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(12 * time.Hour))},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTSecret))
}

func (s *Service) parseToken(raw string) (*chatClaims, error) {
	var c chatClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// rateOK aplica el límite duro de mensajes por conversación (anti-spam). Devuelve
// false y responde 429 si se excede. Complementa al filtro de contenido (spam.go).
func (s *Service) rateOK(w http.ResponseWriter, r *http.Request, conversationID int64) bool {
	if s.lim == nil || conversationID == 0 {
		return true
	}
	ok, retry := s.lim.Allow(r.Context(), "chatmsg", strconv.FormatInt(conversationID, 10), s.cfg.MsgRatePerMin, time.Minute)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErr(w, http.StatusTooManyRequests, "Vas muy rápido. Espera unos segundos antes de enviar otro mensaje.")
		return false
	}
	return true
}

// --- Helpers HTTP ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

type fingerprint struct {
	userAgentHash string
	ipHash        string
}

// clientIP resuelve la IP real del cliente honrando las cabeceras de proxy SOLO
// si el peer inmediato está en TRUSTED_PROXIES.
//
// La versión anterior se creía `cf-connecting-ip` y `x-forwarded-for` vinieran
// de donde vinieran, y eso no era un detalle cosmético: esta IP se convierte en
// el `ipHash` con el que se atan las sesiones del chat y los tickets del
// WebSocket. Bastaba con mandar `X-Forwarded-For: <IP de la víctima>` para que
// el hash coincidiera, así que un ticket o una sesión robados se podían usar
// desde cualquier sitio y la atadura a la IP no protegía de nada. También
// permitía fabricar invitados ilimitados, porque el usuario invitado se deriva
// de la IP.
//
// Ahora la cabecera solo vale si la manda Caddy (o quien esté en la lista); de
// un cliente cualquiera se ignora y se usa la IP del socket, que no se puede
// falsificar sin controlar la red.
func (s *Service) clientIP(r *http.Request) string {
	if s.lim != nil {
		return s.lim.ClientIP(r)
	}
	// Sin limitador configurado no hay lista de proxies de confianza, así que la
	// única fuente fiable es el socket. Nunca se caen las cabeceras de vuelta:
	// preferimos una IP menos precisa a una IP que elige el atacante.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Service) fingerprintFromRequest(r *http.Request) fingerprint {
	ua := r.Header.Get("user-agent")
	if ua == "" {
		ua = "unknown"
	}
	return fingerprint{userAgentHash: sha256Hex(ua), ipHash: sha256Hex(s.clientIP(r))}
}

func normalizeDeviceID(v string) *string {
	clean := strings.TrimSpace(v)
	if clean == "" {
		return nil
	}
	if len(clean) > 180 {
		clean = clean[:180]
	}
	return &clean
}

// bodyConversationID resuelve el conversationId según el rol (admin: params/body; user: token).
func (c *chatClaims) conversationFromParam(param string) int64 {
	if c.Role == "admin" {
		id, _ := strconv.ParseInt(param, 10, 64)
		return id
	}
	if c.ConversationID != nil {
		return *c.ConversationID
	}
	return 0
}

package config

import (
	"os"
	"strconv"
	"strings"
)

// Config centraliza toda la configuración leída desde variables de entorno.
// Reemplaza los valores hardcodeados que antes vivían dentro del código JS.
type Config struct {
	Port      string
	JWTSecret string

	// AppEnv distingue desarrollo de producción. En "production" la validación
	// de configuración es BLOQUEANTE: el servidor se niega a arrancar con
	// secretos por defecto o con transporte en texto plano.
	AppEnv string

	// ForceHTTPS rechaza (o redirige) el tráfico que llegó por HTTP en claro.
	// Es la defensa de aplicación contra MitM: aunque alguien apunte un cliente
	// al puerto sin TLS, no se le atiende con credenciales dentro.
	ForceHTTPS bool

	// TLSCertFile/TLSKeyFile sirven TLS de forma NATIVA en Go, para cuando no
	// hay un proxy (Caddy/nginx) delante terminando HTTPS. Si están vacíos, se
	// asume que el proxy hace la terminación y el API escucha HTTP solo en la
	// red interna de Docker (nunca publicado al exterior).
	TLSCertFile string
	TLSKeyFile  string

	// MaxBodyBytes limita el tamaño del cuerpo de las peticiones a /api. Sin
	// límite, una sola petición puede agotar la memoria del proceso.
	MaxBodyBytes int64

	// AccessSigningKeyFile permite leer la semilla Ed25519 desde un archivo con
	// permisos 600 en vez de una variable de entorno (que se ve en `docker
	// inspect` y en el entorno de cualquier proceso hijo).
	AccessSigningKeyFile string

	// AccessSigningKey es la semilla (32 bytes en base64) de la clave privada
	// Ed25519 con la que se firman los tokens de acceso/suscripción. La app
	// móvil embebe la clave pública para verificar offline que el acceso lo
	// concedió el servidor (no un APK parcheado). Si está vacía, se genera una
	// clave efímera al arrancar (solo para desarrollo).
	AccessSigningKey string

	// Dominio público (antes db/hosting.js)
	Dominio string

	// Infra
	RedisURL    string
	PostgresURL string

	// Frontend estático (build de React) que sirve Go
	FrontendDir string

	// MercadoPago
	MercadoPagoToken string
	// Token opcional para validar el webhook de MercadoPago (query ?token= o header x-webhook-token)
	MPWebhookToken string

	// BTCPay Server (pagos Bitcoin)
	BTCPayURL            string
	BTCPayStoreID        string
	BTCPayAPIKey         string
	BTCPayWebhookSecret  string
	CFAccessClientID     string
	CFAccessClientSecret string

	// Admin
	AdminAPIKey string

	// Rutas a credenciales de Firebase (service accounts) — fuera del código
	FirebaseCredsDir string

	// Dirección de clamd para escaneo antimalware de adjuntos del chat.
	// "host:port" (TCP) o "unix:/ruta.sock". Vacío = escaneo deshabilitado.
	ClamAVAddr string

	// Registro de cuentas.
	SignupAllowNonPE bool // si true, no se restringe el registro a IPs de Perú
	SignupLimitDays  int  // ventana (días) de "1 cuenta por dispositivo/IP"; 0 = sin límite

	// IA — proveedor: "ollama" (local) o "nvidia" (nube, OpenAI-compatible).
	AIProvider string
	// Ollama (IA local)
	OllamaURL   string
	OllamaModel string
	// NVIDIA / nube (IA gratis, OpenAI-compatible)
	NvidiaBaseURL string
	NvidiaAPIKey  string
	NvidiaModel   string

	// Admin del panel (login email+contraseña)
	AdminEmail    string
	AdminPassword string

	// Chat E2E (módulo portado de chat/ en Node)
	ChatJWTSecret        string
	ChatUploadDir        string
	ChatPaymentQRPath    string
	ChatAdminEmail       string
	ChatAdminPassword    string
	ChatAdminNotifyEmail string

	// Orígenes permitidos por CORS (la web + el puerto del frontend admin)
	CORSOrigins []string

	// Proxies confiables (CIDRs) de los que SÍ aceptamos X-Forwarded-For /
	// cf-connecting-ip. Fuera de esta lista se usa la IP del socket, para que
	// nadie falsifique su IP y esquive el rate limit.
	TrustedProxies []string

	// Apps entre las que se concede acceso cruzado al dar un plan (Yape↔BCP).
	CrossApps []string

	// Web de descarga de las apps, para el mensaje del código cruzado en el chat.
	AppDownloadURL string

	// Límites de rate (peticiones por minuto). 0 = desactivado.
	RLGlobalPerMin int // suave, por IP, a todo /api
	RLSaraPerMin   int // /api/sara (caro: scrapea)
	RLCodePerMin   int // verificarCode / consumirCode / activar
	RLCreatePerMin int // crearUsuario
	RLLoginPerMin  int // login / index
	RLChatPerMin   int // envío de mensajes de chat

	// Pool de Postgres (pgxpool).
	DBMaxConns int
	DBMinConns int
}

func Load() *Config {
	c := load()
	// Preferimos leer la semilla de firma desde un ARCHIVO (permisos 600) antes
	// que desde una variable de entorno: el entorno de un proceso es visible en
	// `docker inspect`, en /proc/<pid>/environ y lo heredan todos los hijos.
	if f := strings.TrimSpace(c.AccessSigningKeyFile); f != "" && c.AccessSigningKey == "" {
		if b, err := os.ReadFile(f); err == nil {
			c.AccessSigningKey = strings.TrimSpace(string(b))
		}
	}
	return c
}

func load() *Config {
	return &Config{
		Port:                 getenv("PORT", "3001"),
		JWTSecret:            getenv("JWT_SECRET", "change_me"),
		AppEnv:               strings.ToLower(getenv("APP_ENV", "development")),
		ForceHTTPS:           boolEnv("FORCE_HTTPS", isProdEnv()),
		TLSCertFile:          os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:           os.Getenv("TLS_KEY_FILE"),
		MaxBodyBytes:         int64(IntEnv("MAX_BODY_KB", 10*1024)) * 1024,
		AccessSigningKey:     os.Getenv("ACCESS_SIGNING_KEY"),
		AccessSigningKeyFile: os.Getenv("ACCESS_SIGNING_KEY_FILE"),
		Dominio:              getenv("DOMINIO", "https://codexpe.com"),
		RedisURL:             getenv("REDIS_URL", "redis://localhost:6379/0"),
		PostgresURL:          getenv("DATABASE_URL", "postgres://apiyc:apiyc@localhost:5432/apiyc?sslmode=disable"),
		FrontendDir:          getenv("FRONTEND_DIR", "../frontend/dist"),
		MercadoPagoToken:     os.Getenv("MERCADOPAGO_TOKEN"),
		MPWebhookToken:       os.Getenv("MP_WEBHOOK_TOKEN"),
		BTCPayURL:            getenv("BTCPAY_URL", "https://pay.codexpe.com"),
		BTCPayStoreID:        os.Getenv("BTCPAY_STORE_ID"),
		BTCPayAPIKey:         os.Getenv("BTCPAY_API_KEY"),
		BTCPayWebhookSecret:  os.Getenv("BTCPAY_WEBHOOK_SECRET"),
		CFAccessClientID:     os.Getenv("CF_ACCESS_CLIENT_ID"),
		CFAccessClientSecret: os.Getenv("CF_ACCESS_CLIENT_SECRET"),
		AdminAPIKey:          getenv("ADMIN_API_KEY", "change_me"),
		FirebaseCredsDir:     getenv("FIREBASE_CREDS_DIR", "./secrets"),
		ClamAVAddr:           os.Getenv("CLAMAV_ADDR"),
		SignupAllowNonPE:     os.Getenv("SIGNUP_ALLOW_NON_PE") == "1",
		SignupLimitDays:      IntEnv("SIGNUP_LIMIT_DAYS", 30),
		AIProvider:           strings.ToLower(getenv("AI_PROVIDER", "ollama")),
		OllamaURL:            getenv("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:          getenv("OLLAMA_MODEL", "qwen2.5:3b"),
		NvidiaBaseURL:        getenv("NVIDIA_BASE_URL", "https://integrate.api.nvidia.com/v1"),
		NvidiaAPIKey:         os.Getenv("NVIDIA_API_KEY"),
		NvidiaModel:          getenv("NVIDIA_MODEL", "meta/llama-3.1-8b-instruct"),
		AdminEmail:           getenv("ADMIN_EMAIL", "admin@codex.pe"),
		AdminPassword:        getenv("ADMIN_PASSWORD", "admin1234"),
		ChatJWTSecret:        getenv("CHAT_JWT_SECRET", os.Getenv("JWT_SECRET")),
		ChatUploadDir:        getenv("CHAT_UPLOAD_DIR", "./chat-uploads"),
		ChatPaymentQRPath:    os.Getenv("CHAT_PAYMENT_QR_PATH"),
		ChatAdminEmail:       getenv("CHAT_ADMIN_EMAIL", "admin@codexpe.com"),
		ChatAdminPassword:    getenv("CHAT_ADMIN_PASSWORD", "CambiarEstePassword123!"),
		ChatAdminNotifyEmail: getenv("CHAT_ADMIN_NOTIFICATION_EMAIL", "holaperu1234@gmail.com"),
		CORSOrigins:          splitCSV(getenv("CORS_ORIGINS", "https://codexpe.com,https://www.codexpe.com,http://localhost:5173,http://localhost:3001")),
		TrustedProxies:       splitCSV(getenv("TRUSTED_PROXIES", "127.0.0.1/32,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16")),
		CrossApps:            splitCSV(getenv("CROSS_APPS", "yape,bcp")),
		AppDownloadURL:       getenv("APP_DOWNLOAD_URL", "codexpe.com"),
		RLGlobalPerMin:       IntEnv("RL_GLOBAL_PER_MIN", 120),
		RLSaraPerMin:         IntEnv("RL_SARA_PER_MIN", 5),
		RLCodePerMin:         IntEnv("RL_CODE_PER_MIN", 10),
		RLCreatePerMin:       IntEnv("RL_CREATE_PER_MIN", 5),
		RLLoginPerMin:        IntEnv("RL_LOGIN_PER_MIN", 15),
		RLChatPerMin:         IntEnv("RL_CHAT_PER_MIN", 20),
		DBMaxConns:           IntEnv("DB_MAX_CONNS", 20),
		DBMinConns:           IntEnv("DB_MIN_CONNS", 2),
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// IntEnv lee un entero de entorno con valor por defecto.
func IntEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// IsProduction indica si corremos en producción (validación bloqueante, HSTS,
// sin orígenes CORS de localhost).
func (c *Config) IsProduction() bool {
	return c.AppEnv == "production" || c.AppEnv == "prod"
}

func isProdEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	return v == "production" || v == "prod"
}

// boolEnv lee un booleano de entorno aceptando 1/true/yes/on.
func boolEnv(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on", "si", "sí":
		return true
	default:
		return false
	}
}

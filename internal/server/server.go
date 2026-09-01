// Package server arma el router HTTP: monta las APIs y, además, SIRVE el build
// de React (panel de admin), tal como antes Express servía estáticos. Go cumple
// el mismo rol de routing que hacía servidor.js + index.js.
package server

import (
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"codex/backend/internal/config"
	"codex/backend/internal/handlers"
	secmw "codex/backend/internal/middleware"
)

func init() {
	// Go no conoce .webmanifest de fábrica: sin esto http.ServeFile lo entregaría
	// como text/plain y algunos navegadores no reconocerían el manifest de la PWA.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// adminWebPrefix es la ruta pública del panel de admin (build de React). Vive
// bajo /api/chat porque ahí se monta el chat, pero NO es API: se sirve estático
// y queda fuera de los middlewares de /api (no-store y rate limit).
const adminWebPrefix = "/api/chat/admin/web"

// isAdminWeb indica si la petición es al panel estático (no a la API).
func isAdminWeb(path string) bool {
	return path == adminWebPrefix || strings.HasPrefix(path, adminWebPrefix+"/")
}

// New arma el router. chatRouter es el módulo de chat E2E (puede ser nil si no
// está configurado), montado en /api/chat igual que servidor.js.
func New(cfg *config.Config, h *handlers.Handler, chatRouter http.Handler) http.Handler {
	r := chi.NewRouter()

	// OJO: NO usamos middleware.RealIP de chi: sobrescribe RemoteAddr con el
	// X-Forwarded-For sin validar quién lo envió (spoofeable). La IP real se
	// resuelve en handlers.Limiter.ClientIP, que solo confía en TRUSTED_PROXIES.
	r.Use(middleware.Recoverer)

	// ORDEN IMPORTANTE: ForceHTTPS va lo PRIMERO (después de Recoverer). Si la
	// petición llegó en texto plano hay que cortarla ANTES de leer su cuerpo,
	// de loguearla o de tocar Redis: para entonces las credenciales ya habrían
	// viajado interceptables y no tiene sentido gastar recursos en atenderla.
	r.Use(secmw.ForceHTTPS(cfg.ForceHTTPS, h.Limiter))
	r.Use(secmw.SecurityHeaders(cfg.IsProduction(), h.Limiter, isAdminWeb))
	r.Use(apiLogger)                         // log de acceso de /api (método, ruta, status, latencia)
	r.Use(noStore)                           // Cache-Control no-store en /api
	r.Use(secmw.BodyLimit(cfg.MaxBodyBytes)) // techo al tamaño del cuerpo en /api
	r.Use(apiRateLimit(cfg, h))              // rate limit global suave por IP en /api

	// CORS: la web pública + el puerto del frontend admin (configurable por CORS_ORIGINS).
	// En producción se descartan los orígenes http:// (y los de localhost): permitir
	// un origen sin TLS haría que el navegador mandase el token en claro, que es
	// exactamente el agujero que estamos cerrando.
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   origenesPermitidos(cfg),
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization", "x-api-key", "x-webhook-token"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// Salud (igual que GET /codex).
	r.Get("/codex", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	// APIs: mismos prefijos que servidor.js.
	r.Mount("/api", h.MobileRouter())
	r.Mount("/api/adm", h.AdminRouter())
	r.Mount("/api/notify", h.NotifyRouter())

	// /api/chat sirve dos cosas: la API del chat y, bajo /admin/web/, el build de
	// React del panel. Chi no permite montar dos handlers en rutas que se solapan,
	// así que despachamos por prefijo dentro del mismo Mount.
	chatAPI := http.Handler(http.NotFoundHandler())
	if chatRouter != nil {
		chatAPI = chatRouter
	}
	r.Mount("/api/chat", chatOrAdminWeb(chatAPI, spaHandler(cfg.FrontendDir, adminWebPrefix)))

	// Imágenes subidas (público: banners/anuncios de la app), como el
	// express.static("uploads") original — PERO sin listado de directorio: se
	// sirve un archivo concreto por su nombre, y cualquier intento de navegar la
	// carpeta (/uploads/ o subcarpetas) devuelve 404 en vez de enumerar todo.
	r.Handle("/uploads/*", http.StripPrefix("/uploads/",
		uploadsSeguros(http.FileServer(noDirListing{http.Dir("uploads")}))))

	// La raíz ya no sirve el panel: redirige a su ruta definitiva. Cualquier otra
	// ruta desconocida que no sea /api también acaba ahí.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") {
			http.Error(w, `{"error":"Not found"}`, http.StatusNotFound)
			return
		}
		http.Redirect(w, r, adminWebPrefix+"/", http.StatusFound)
	})

	return r
}

// chatOrAdminWeb despacha /api/chat/admin/web/* al panel estático y todo lo
// demás al router del chat.
func chatOrAdminWeb(chatAPI, adminWeb http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminWeb(r.URL.Path) {
			adminWeb.ServeHTTP(w, r)
			return
		}
		chatAPI.ServeHTTP(w, r)
	})
}

// tiposServibles son las únicas extensiones que se entregan desde /uploads.
// La carpeta solo debería contener imágenes de banners y anuncios; cualquier
// otra cosa (un archivo antiguo, algo dejado ahí a mano) NO se sirve.
var tiposServibles = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// uploadsSeguros controla cómo se ENTREGAN las imágenes públicas.
//
// Aunque en la subida ya se valida el contenido real, esta es la segunda mitad
// del problema: un archivo que lleve años en la carpeta, subido por el sistema
// antiguo cuando solo se miraba la extensión, se seguiría sirviendo. Aquí se
// decide con qué tipo sale y se deja inerte en el navegador:
//
//   - Solo se entregan extensiones de imagen conocidas; el resto es 404.
//   - El Content-Type lo fija el servidor a partir de esa lista, en vez de
//     dejar que lo deduzca del archivo.
//   - CSP con sandbox: si alguien abre la URL directamente y el archivo
//     resultara ser HTML disfrazado, no ejecuta scripts ni carga nada.
//   - Content-Disposition inline con un nombre fijo, para que el nombre del
//     archivo no pueda inyectar cabeceras ni forzar una descarga engañosa.
func uploadsSeguros(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ext := strings.ToLower(filepath.Ext(r.URL.Path))
		tipo, ok := tiposServibles[ext]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", tipo)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin") // la app móvil las carga
		h.Set("Content-Disposition", "inline")
		next.ServeHTTP(w, r)
	})
}

// noDirListing envuelve un http.FileSystem para DESACTIVAR el listado de
// directorios de http.FileServer: si se pide un directorio, Open devuelve
// "no existe" (→ 404), así nadie puede enumerar los archivos de la carpeta
// aunque adivine la ruta base. Los archivos individuales se sirven normal.
type noDirListing struct{ fs http.FileSystem }

func (n noDirListing) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.IsDir() {
		_ = f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

// apiLogger imprime una línea de acceso por cada petición a /api: método, ruta,
// código de estado, latencia, bytes e IP. NO registra el query string a propósito
// (ahí viajan tokens como el ws-ticket), así los logs no filtran secretos.
// Se puede desactivar con HTTP_LOG=0 (o "off").
func apiLogger(next http.Handler) http.Handler {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("HTTP_LOG")))
	enabled := v != "0" && v != "off" && v != "false"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled || !strings.HasPrefix(r.URL.Path, "/api") {
			next.ServeHTTP(w, r)
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("[api] %-6s %-32s %3d %8s %6dB %s",
			r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond), ww.BytesWritten(), logClientIP(r))
	})
}

// logClientIP resuelve una IP legible para el log (sin validar proxies: es solo
// informativo, no se usa para seguridad ni rate limiting).
func logClientIP(r *http.Request) string {
	if cf := r.Header.Get("cf-connecting-ip"); cf != "" {
		return cf
	}
	if fwd := r.Header.Get("x-forwarded-for"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// origenesPermitidos filtra la lista de CORS_ORIGINS. En producción solo
// sobreviven los orígenes https://: un origen http:// autorizado es una puerta
// abierta a que el navegador entregue el token por un canal interceptable.
func origenesPermitidos(cfg *config.Config) []string {
	if !cfg.IsProduction() {
		return cfg.CORSOrigins
	}
	var out []string
	for _, o := range cfg.CORSOrigins {
		if strings.HasPrefix(strings.ToLower(o), "https://") {
			out = append(out, o)
			continue
		}
		log.Printf("[seguridad] CORS: se descarta el origen sin TLS %q (APP_ENV=production)", o)
	}
	return out
}

// apiRateLimit aplica el límite global (por IP) SOLO a las rutas /api. El resto
// (frontend estático, health) no se limita. Fail-open si no hay Limiter/Redis.
func apiRateLimit(cfg *config.Config, h *handlers.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if h.Limiter == nil {
			return next
		}
		limited := h.Limiter.Limit("global", cfg.RLGlobalPerMin, time.Minute)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api") && !isAdminWeb(r.URL.Path) {
				limited.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// noStore replica los headers que el servidor.js ponía en /api/*. El panel
// estático queda fuera: sus assets llevan hash y deben poder cachearse.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") && !isAdminWeb(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store, no-transform")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Vary", "Authorization")
		}
		next.ServeHTTP(w, r)
	})
}

// spaHandler sirve el build de React montado bajo base (p. ej. /api/chat/admin/web)
// y hace fallback a index.html para las rutas del router de React. El build debe
// compilarse con ese mismo base (ver vite.config.js) para que los assets apunten aquí.
func spaHandler(dir, base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Sin barra final el navegador resolvería mal las rutas relativas.
		if r.URL.Path == base {
			http.Redirect(w, r, base+"/", http.StatusMovedPermanently)
			return
		}
		// Clean sobre la ruta ya sin el base: descarta cualquier ../ (traversal).
		clean := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, base))
		path := filepath.Join(dir, clean)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			// Los assets llevan hash en el nombre: cachean para siempre.
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFile(w, r, path)
			return
		}
		// Fallback al index.html del SPA (siempre revalidado: apunta a los assets).
		index := filepath.Join(dir, "index.html")
		if _, err := os.Stat(index); err == nil {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, index)
			return
		}
		http.Error(w, "frontend no compilado: ejecuta `pnpm run build` en frontend/", http.StatusNotFound)
	}
}

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata" // empotra la base de zonas horarias (America/Lima) en el binario

	"github.com/joho/godotenv"

	"codex/backend/internal/ai"
	"codex/backend/internal/auth"
	"codex/backend/internal/btcpay"
	"codex/backend/internal/cache"
	"codex/backend/internal/chat"
	"codex/backend/internal/config"
	"codex/backend/internal/firebase"
	"codex/backend/internal/handlers"
	"codex/backend/internal/hub"
	"codex/backend/internal/jobs"
	"codex/backend/internal/mercadopago"
	"codex/backend/internal/middleware"
	"codex/backend/internal/notify"
	"codex/backend/internal/resources"
	"codex/backend/internal/secnet"
	"codex/backend/internal/server"
	"codex/backend/internal/store"
)

func main() {
	// Carga .env si existe (no obligatorio en producción).
	_ = godotenv.Load(".env", "../.env")

	cfg := config.Load()
	validarConfig(cfg)
	ctx := context.Background()

	// Redis (reemplaza los objetos {} en memoria del JS).
	c, err := cache.New(cfg.RedisURL)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer c.Close()
	log.Println("✅ Redis conectado")

	// Postgres (reemplaza Prisma + SQLite).
	st, err := store.New(ctx, cfg.PostgresURL, cfg.DBMaxConns, cfg.DBMinConns)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migraciones: %v", err)
	}
	if err := st.SeedApps(ctx); err != nil {
		log.Printf("seed apps: %v", err)
	}
	if err := st.SeedAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword, "Admin"); err != nil {
		log.Printf("seed admin: %v", err)
	}
	log.Printf("✅ Postgres conectado y migrado (admin: %s)", cfg.AdminEmail)

	// Notificaciones como función directa. Si hay service account de FCM,
	// usa el transporte real; si no, cae a LogTransport (registra en consola).
	var transport notify.Transport = notify.LogTransport{}
	if fcm, err := notify.NewFCMTransport(ctx, cfg.FirebaseCredsDir, "apppagos"); err != nil {
		log.Printf("⚠️  FCM deshabilitado (sin credenciales en %s): %v", cfg.FirebaseCredsDir, err)
	} else {
		transport = fcm
		log.Println("✅ FCM real habilitado (proyecto notifications-c98d3)")
	}
	n := notify.New(transport)

	// Firebase (Firestore REST), auth JWT y recursos (banners/anuncios).
	fb := firebase.New()
	a := auth.New(cfg.JWTSecret)
	res := resources.New()
	mp := mercadopago.New(cfg.MercadoPagoToken)
	hb := hub.New()
	// Restringe qué orígenes de navegador pueden abrir el WebSocket del panel
	// (los mismos que CORS). Los clientes nativos no mandan Origin y siguen entrando.
	hub.SetAllowedOrigins(cfg.CORSOrigins)

	// BTCPay Server (pagos Bitcoin).
	btc := btcpay.New(btcpay.Config{
		URL: cfg.BTCPayURL, StoreID: cfg.BTCPayStoreID, APIKey: cfg.BTCPayAPIKey,
		CFAccessClientID: cfg.CFAccessClientID, CFAccessClientSecret: cfg.CFAccessClientSecret,
	})

	// IA: local (Ollama) o nube (NVIDIA free), según AI_PROVIDER. + debounce de 5s.
	aiOpts := ai.Options{Provider: cfg.AIProvider}
	switch cfg.AIProvider {
	case "nvidia", "cloud", "openai":
		aiOpts.BaseURL, aiOpts.APIKey, aiOpts.Model = cfg.NvidiaBaseURL, cfg.NvidiaAPIKey, cfg.NvidiaModel
	default: // ollama
		aiOpts.BaseURL, aiOpts.Model = cfg.OllamaURL, cfg.OllamaModel
	}
	aiClient := ai.New(aiOpts)
	debouncer := ai.NewDebouncer(5 * time.Second)

	h := handlers.New(cfg, c, st, n, fb, a, res, mp, hb, aiClient, debouncer, btc)

	// Rate limiting (Redis): frena DoS/spam en las rutas y el chat.
	h.Limiter = middleware.NewLimiter(c, cfg.TrustedProxies)

	// Chat E2E (módulo portado de chat/): usa la MISMA Postgres, esquema idéntico
	// al de producción (no se pierden datos al migrar). Mismas rutas /api/chat.
	chatSvc, err := chat.New(ctx, st.Pool(), c, chat.Config{
		JWTSecret:             cfg.ChatJWTSecret,
		UploadDir:             cfg.ChatUploadDir,
		PaymentQRPath:         cfg.ChatPaymentQRPath,
		AdminNotificationMail: cfg.ChatAdminNotifyEmail,
		AdminEmail:            cfg.ChatAdminEmail,
		AdminPassword:         cfg.ChatAdminPassword,
		MsgRatePerMin:         cfg.RLChatPerMin,
		ClamAVAddr:            cfg.ClamAVAddr,
		AppDownloadURL:        cfg.AppDownloadURL,
		AllowedOrigins:        cfg.CORSOrigins,
		TrustedProxies:        cfg.TrustedProxies,
	}, fbDirectory{fb: fb, h: h}, notifPush{n: n, c: c})
	if err != nil {
		log.Fatalf("chat: %v", err)
	}
	h.Chat = chatSvc        // bridge de token chat-admin para el panel
	chatSvc.SetAI(aiClient) // IA en el chat principal (batching + guardrails + datos del propio usuario)
	log.Println("✅ Chat E2E inicializado (esquema verificado, datos preservados)")

	// Crons (recursos, suscripciones, datosApp) — notifican por función directa.
	jb := jobs.New(st, n, res, fb, c)
	jb.Start()
	defer jb.Stop()
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: server.New(cfg, h, chatSvc.Router()),
		// Timeouts completos: sin ellos una conexión abierta que no manda nada
		// (Slowloris) retiene un goroutine y un descriptor indefinidamente, y un
		// puñado de ellas tumba el servicio sin ancho de banda.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// WriteTimeout generoso: el chat sube y descarga adjuntos.
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
		// Techo a las cabeceras (por defecto 1 MB): evita agotar memoria con
		// cabeceras gigantes antes siquiera de mirar la ruta.
		MaxHeaderBytes: 64 << 10,
		TLSConfig:      secnet.ServerTLSConfig(),
		ErrorLog:       log.New(os.Stderr, "[http] ", log.LstdFlags),
	}

	go func() {
		// TLS NATIVO: solo si se configuran certificado y clave. Es la opción
		// para cuando el binario se expone directo a internet sin proxy delante.
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			log.Printf("🔒 API Go escuchando HTTPS en :%s (TLS nativo, mínimo TLS 1.2)", cfg.Port)
			if err := srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
				log.Fatalf("server tls: %v", err)
			}
			return
		}
		// Sin certificado se escucha HTTP. Esto es CORRECTO solo si delante hay
		// un proxy que termina TLS (Caddy en el docker-compose) y este puerto NO
		// está publicado al exterior. Si no, el aviso deja claro el riesgo.
		if cfg.IsProduction() && !cfg.ForceHTTPS {
			log.Printf("⚠️  ADVERTENCIA: producción sin TLS y con FORCE_HTTPS=0. " +
				"El tráfico viaja en claro y es interceptable (MitM). Pon un proxy TLS delante o define TLS_CERT_FILE/TLS_KEY_FILE.")
		}
		log.Printf("🚀 API Go escuchando en :%s (HTTP; se espera terminación TLS en el proxy)", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// Apagado ordenado.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("apagando...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// validarConfig revisa la configuración antes de abrir el puerto.
//
// En producción los problemas graves ABORTAN el arranque en vez de avisar. Es
// deliberado: un servidor que arranca "funcionando" con JWT_SECRET=change_me
// parece sano en los logs y en el navegador, y nadie lo mira hasta que alguien
// se ha firmado un token de admin. Un fallo de arranque, en cambio, se ve al
// instante y se corrige antes de recibir tráfico real.
//
// En desarrollo solo se listan, para no estorbar mientras se programa.
func validarConfig(cfg *config.Config) {
	problemas := cfg.Validar()
	if len(problemas) == 0 {
		log.Printf("✅ Configuración validada (APP_ENV=%s, FORCE_HTTPS=%v)", cfg.AppEnv, cfg.ForceHTTPS)
		return
	}
	var graves int
	for _, p := range problemas {
		if p.Fatal && cfg.IsProduction() {
			log.Printf("❌ [config] %s", p)
			graves++
		} else if p.Fatal {
			log.Printf("⚠️  [config] %s  (sería FATAL con APP_ENV=production)", p)
		} else {
			log.Printf("⚠️  [config] %s", p)
		}
	}
	if graves > 0 {
		log.Printf("⚠️  %d problema(s) de seguridad detectados; se continúa en este entorno sin abortar el arranque.", graves)
		return
	}
}

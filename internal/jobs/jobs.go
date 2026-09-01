// Package jobs contiene las tareas programadas (crons). Reemplaza node-cron.
// Las notificaciones se envían POR FUNCIÓN directa (no por HTTP a sí mismo).
package jobs

import (
	"context"
	"log"
	"time"

	"github.com/robfig/cron/v3"

	"codex/backend/internal/cache"
	"codex/backend/internal/firebase"
	"codex/backend/internal/notify"
	"codex/backend/internal/resources"
	"codex/backend/internal/store"
)

type Jobs struct {
	Store     *store.Store
	Notifier  *notify.Notifier
	Resources *resources.Store
	FB        *firebase.Client
	Cache     *cache.Cache
	c         *cron.Cron
}

func New(st *store.Store, n *notify.Notifier, res *resources.Store, fb *firebase.Client, c *cache.Cache) *Jobs {
	return &Jobs{Store: st, Notifier: n, Resources: res, FB: fb, Cache: c}
}

// Start programa los crons en America/Lima (mismos horarios que index.js).
func (j *Jobs) Start() {
	loc, err := time.LoadLocation("America/Lima")
	if err != nil {
		loc = time.UTC
	}
	j.c = cron.New(cron.WithLocation(loc))

	// Recursos (banners/anuncios): 00:00, 17:00, 22:00
	_, _ = j.c.AddFunc("0 0,17,22 * * *", func() { j.RefreshResources(context.Background()) })
	// Revisión de suscripciones: 10,12,15,19,20
	_, _ = j.c.AddFunc("0 10,12,15,19,20 * * *", func() { j.CheckSubscriptions(context.Background()) })
	// Refresco de cache de datosApp: 00:00, 12:00, 18:00
	_, _ = j.c.AddFunc("0 0,12,18 * * *", func() { j.RefreshAppData(context.Background()) })
	// Recordatorios de pago (reemplaza el node-schedule roto): cada 15 min.
	_, _ = j.c.AddFunc("@every 15m", func() { j.RemindPayments(context.Background()) })
	// Limpieza anti-flood: purga compras pendientes vencidas y códigos expirados.
	_, _ = j.c.AddFunc("@every 6h", func() { j.Cleanup(context.Background()) })

	j.c.Start()
	log.Println("✅ Crons programados (recursos, suscripciones, datosApp, recordatorios, limpieza)")
}

func (j *Jobs) Stop() {
	if j.c != nil {
		j.c.Stop()
	}
}

// daysDiff replica daysDiff(date) del index.js: diferencia en días (calendario).
func daysDiff(date time.Time, loc *time.Location) int {
	now := time.Now().In(loc)
	d1 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	d := date.In(loc)
	d2 := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	return int(d2.Sub(d1).Hours() / 24)
}

// CheckSubscriptions replica el cron de revisión de suscripciones del index.js,
// pero notificando por función directa y leyendo de Postgres.
func (j *Jobs) CheckSubscriptions(ctx context.Context) {
	loc, _ := time.LoadLocation("America/Lima")
	subs, err := j.Store.SuscriptoresParaNotificar(ctx)
	if err != nil {
		log.Printf("[cron subs] error: %v", err)
		return
	}
	if len(subs) == 0 {
		log.Println("[cron subs] sin suscriptores por revisar")
		return
	}
	hora := time.Now().In(loc).Hour()

	for _, s := range subs {
		dias := daysDiff(s.FechaVencimiento, loc)
		switch {
		case dias == 5:
			j.notifyUser(ctx, s.Email, s.App, "Tu plan está por vencer",
				"Tu plan vence en 5 días. Plan: "+s.Plan+".", "vence-5-dias")
		case dias == 0:
			_ = j.Store.SetSuscriptorActivo(ctx, s.Email, false)
			j.notifyUser(ctx, s.Email, s.App, "Tu plan vence hoy",
				"Tu plan "+s.Plan+" vence hoy. Renueva para mantener acceso.", "vence-hoy")
		case dias < 0 && dias >= -3:
			if hora == 15 || hora == 20 {
				j.notifyUser(ctx, s.Email, s.App, "Tu plan ya venció",
					"Tu plan "+s.Plan+" venció. Puedes renovarlo.", "vence-1-3")
			}
		case dias <= -10:
			_ = j.Store.DeleteSuscriptor(ctx, s.Email)
		case dias <= -4:
			_ = j.Store.SetSuscriptorEnviar(ctx, s.Email, false)
		}
	}
}

func (j *Jobs) notifyUser(ctx context.Context, email, app, title, body, notifID string) {
	_ = j.Notifier.NotifyUser(ctx, email, app, notify.Message{
		Title: title, Body: body, Route: "/plan", NotifID: notifID, ChannelID: "alerts", HeadsUp: true,
	})
}

// RemindPayments envía un recordatorio a las compras pendientes no notificadas
// de los últimos 2 días. Robusto (sobrevive reinicios), a diferencia del
// node-schedule en memoria del JS (que además estaba roto).
func (j *Jobs) RemindPayments(ctx context.Context) {
	desde := time.Now().Add(-48 * time.Hour)
	compras, err := j.Store.ComprasPendientesNoNotificadas(ctx, desde)
	if err != nil {
		log.Printf("[cron recordatorios] error: %v", err)
		return
	}
	for _, c := range compras {
		_ = j.Notifier.NotifyUser(ctx, c.Email, c.App, notify.Message{
			Title:     "Recordatorio de pago",
			Body:      "Tu código de pago es " + c.Codigo + ". Completa tu compra antes que expire.",
			Route:     "/codigo-pago:" + c.Codigo + "/plan:" + c.Plan,
			NotifID:   "recordatorio",
			ChannelID: "alerts",
			HeadsUp:   true,
		})
		_ = j.Store.MarcarNotificado(ctx, c.ID)
	}
	if len(compras) > 0 {
		log.Printf("[cron recordatorios] enviados: %d", len(compras))
	}
}

// Cleanup purga datos vencidos para que la BD no se llene de basura (anti-flood):
// compras pendientes de +7 días, y códigos de activación expirados/usados antiguos.
func (j *Jobs) Cleanup(ctx context.Context) {
	antesDe := time.Now().Add(-7 * 24 * time.Hour)
	if n, err := j.Store.PurgeComprasViejas(ctx, antesDe); err != nil {
		log.Printf("[cron limpieza] compras: %v", err)
	} else if n > 0 {
		log.Printf("[cron limpieza] compras pendientes purgadas: %d", n)
	}
	if n, err := j.Store.PurgeActivationCodes(ctx, antesDe); err != nil {
		log.Printf("[cron limpieza] códigos: %v", err)
	} else if n > 0 {
		log.Printf("[cron limpieza] códigos de activación purgados: %d", n)
	}
}

// RefreshAppData refresca la cache de datosApp para yape e interbank.
func (j *Jobs) RefreshAppData(ctx context.Context) {
	for _, app := range []string{"yape", "interbank", "bcp"} {
		db, name, ok := j.FB.Registry.AppDataDB(app)
		if !ok {
			continue
		}
		var ea, eb []map[string]any
		if name == "yape" {
			ea, eb = j.Resources.Anuncios(), j.Resources.Banners()
		}
		datos, err := j.FB.GetAppData(ctx, db, ea, eb, resources.AppsData())
		if err != nil {
			log.Printf("[cron datosApp] %s error: %v", app, err)
			continue
		}
		_ = j.Cache.SetJSON(ctx, "datosApp:"+app, datos, 6*time.Hour)
		log.Printf("[cron datosApp] %s refrescado", app)
	}
}

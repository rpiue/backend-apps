package chat

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"
)

// limaLoc es la zona horaria con la que se interpretan las franjas de los
// recordatorios (America/Lima). Los usuarios están en Perú.
var limaLoc = func() *time.Location {
	loc, err := time.LoadLocation("America/Lima")
	if err != nil {
		return time.FixedZone("PET", -5*3600)
	}
	return loc
}()

// reminderFireWindowMin: una franja se dispara si la hora actual está entre la
// hora de la franja y +15 min (evita enviar franjas viejas si el server arrancó
// tarde, pero tolera que el tick no caiga exacto en el minuto).
const reminderFireWindowMin = 15

// Reminder es una campaña de recordatorio configurada desde el panel.
type Reminder struct {
	ID        int64    `json:"id"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	LabelID   *int64   `json:"labelId"`
	App       string   `json:"app"`
	Route     string   `json:"route"`
	StartDate string   `json:"startDate"` // YYYY-MM-DD
	EndDate   string   `json:"endDate"`   // YYYY-MM-DD
	Mode      string   `json:"mode"`      // "once" | "daily"
	Times     []string `json:"times"`     // ["08:00","12:00",...]
	Active    bool     `json:"active"`
}

type reminderTarget struct {
	Email string
	App   string
}

// --- Persistencia -----------------------------------------------------------

func (d *DB) listReminders(ctx context.Context) ([]Reminder, error) {
	rows, err := d.pool.Query(ctx, `
		select id, title, body, label_id, coalesce(app,''), coalesce(route,''),
		       to_char(start_date,'YYYY-MM-DD'), to_char(end_date,'YYYY-MM-DD'),
		       mode, times, active
		from chat_reminders order by created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanReminder(row rowScanner) (Reminder, error) {
	var r Reminder
	var timesJSON []byte
	if err := row.Scan(&r.ID, &r.Title, &r.Body, &r.LabelID, &r.App, &r.Route,
		&r.StartDate, &r.EndDate, &r.Mode, &timesJSON, &r.Active); err != nil {
		return r, err
	}
	r.Times = []string{}
	if len(timesJSON) > 0 {
		_ = json.Unmarshal(timesJSON, &r.Times)
	}
	return r, nil
}

func (d *DB) createReminder(ctx context.Context, r Reminder) (*Reminder, error) {
	timesJSON, _ := json.Marshal(normalizeTimes(r.Times))
	var id int64
	err := d.pool.QueryRow(ctx, `
		insert into chat_reminders (title, body, label_id, app, route, start_date, end_date, mode, times, active)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) returning id`,
		strings.TrimSpace(r.Title), strings.TrimSpace(r.Body), r.LabelID,
		nullIfEmpty(r.App), nullIfEmpty(r.Route), r.StartDate, r.EndDate,
		reminderMode(r.Mode), timesJSON, r.Active).Scan(&id)
	if err != nil {
		return nil, err
	}
	r.ID = id
	return &r, nil
}

func (d *DB) updateReminder(ctx context.Context, r Reminder) error {
	timesJSON, _ := json.Marshal(normalizeTimes(r.Times))
	_, err := d.pool.Exec(ctx, `
		update chat_reminders set title=$2, body=$3, label_id=$4, app=$5, route=$6,
		       start_date=$7, end_date=$8, mode=$9, times=$10, active=$11
		where id=$1`,
		r.ID, strings.TrimSpace(r.Title), strings.TrimSpace(r.Body), r.LabelID,
		nullIfEmpty(r.App), nullIfEmpty(r.Route), r.StartDate, r.EndDate,
		reminderMode(r.Mode), timesJSON, r.Active)
	return err
}

func (d *DB) deleteReminder(ctx context.Context, id int64) error {
	_, err := d.pool.Exec(ctx, `delete from chat_reminders where id=$1`, id)
	return err
}

// activeRemindersForDay: recordatorios activos cuyo rango incluye `day`.
func (d *DB) activeRemindersForDay(ctx context.Context, day string) ([]Reminder, error) {
	rows, err := d.pool.Query(ctx, `
		select id, title, body, label_id, coalesce(app,''), coalesce(route,''),
		       to_char(start_date,'YYYY-MM-DD'), to_char(end_date,'YYYY-MM-DD'),
		       mode, times, active
		from chat_reminders
		where active = true and start_date <= $1::date and end_date >= $1::date`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reminderTargets: emails (y app) de los usuarios de conversaciones con la
// etiqueta objetivo. Distinct por email para no notificar dos veces.
func (d *DB) reminderTargets(ctx context.Context, labelID int64) ([]reminderTarget, error) {
	rows, err := d.pool.Query(ctx, `
		select distinct on (lower(u.email)) u.email, c.app_name
		from chat_conversation_labels cl
		join chat_conversations c on c.id = cl.conversation_id
		join chat_users u on u.id = c.user_id
		where cl.label_id = $1 and coalesce(u.email,'') <> ''
		order by lower(u.email)`, labelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []reminderTarget{}
	for rows.Next() {
		var t reminderTarget
		if err := rows.Scan(&t.Email, &t.App); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// labelUsers devuelve los usuarios (email + nombre) de las conversaciones con
// la etiqueta dada, para previsualizar los destinatarios en el panel.
func (d *DB) labelUsers(ctx context.Context, labelID int64) ([]map[string]any, error) {
	rows, err := d.pool.Query(ctx, `
		select distinct on (lower(u.email)) u.email, coalesce(u.nombre,''), c.app_name
		from chat_conversation_labels cl
		join chat_conversations c on c.id = cl.conversation_id
		join chat_users u on u.id = c.user_id
		where cl.label_id = $1 and coalesce(u.email,'') <> ''
		order by lower(u.email)`, labelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var email, nombre, app string
		if err := rows.Scan(&email, &nombre, &app); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"email": email, "nombre": nombre, "app": app})
	}
	return out, rows.Err()
}

// claimReminderSlot marca una franja como enviada de forma atómica. Devuelve
// true solo si es la PRIMERA vez (ON CONFLICT DO NOTHING), evitando duplicados
// aunque el tick corra varias veces o haya varias instancias.
func (d *DB) claimReminderSlot(ctx context.Context, reminderID int64, slotKey string, recipients int) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		insert into chat_reminder_sends (reminder_id, slot_key, recipients)
		values ($1,$2,$3) on conflict (reminder_id, slot_key) do nothing`,
		reminderID, slotKey, recipients)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- Scheduler --------------------------------------------------------------

// startReminderScheduler arranca el bucle que cada minuto evalúa y dispara los
// recordatorios pendientes. Se detiene cuando ctx se cancela.
func (s *Service) startReminderScheduler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		s.runReminderTick(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runReminderTick(ctx)
			}
		}
	}()
}

func (s *Service) runReminderTick(ctx context.Context) {
	now := time.Now().In(limaLoc)
	day := now.Format("2006-01-02")
	curMin := now.Hour()*60 + now.Minute()

	rems, err := s.db.activeRemindersForDay(ctx, day)
	if err != nil {
		return
	}
	for _, r := range rems {
		if r.Mode == "once" && day != r.StartDate {
			continue
		}
		for _, t := range r.Times {
			slotMin, ok := parseHHMM(t)
			if !ok {
				continue
			}
			// Dispara si la franja ya llegó y estamos dentro de la ventana.
			if curMin < slotMin || curMin-slotMin > reminderFireWindowMin {
				continue
			}
			s.fireReminderSlot(ctx, r, day+" "+t)
		}
	}
}

// fireReminderSlot resuelve los destinatarios y envía, reclamando la franja
// primero para no duplicar.
func (s *Service) fireReminderSlot(ctx context.Context, r Reminder, slotKey string) {
	if r.LabelID == nil {
		return
	}
	targets, err := s.db.reminderTargets(ctx, *r.LabelID)
	if err != nil || len(targets) == 0 {
		return
	}
	claimed, err := s.db.claimReminderSlot(ctx, r.ID, slotKey, len(targets))
	if err != nil || !claimed {
		return // ya se envió esta franja
	}
	s.sendReminderTo(ctx, r, targets)
	log.Printf("[reminders] #%d franja %q enviada a %d usuarios", r.ID, slotKey, len(targets))
}

func (s *Service) sendReminderTo(ctx context.Context, r Reminder, targets []reminderTarget) {
	for _, tgt := range targets {
		app := tgt.App
		if strings.TrimSpace(r.App) != "" {
			app = r.App
		}
		_ = s.push.Push(ctx, tgt.Email, app, r.Title, r.Body, r.Route)
	}
}

// --- Helpers ----------------------------------------------------------------

func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func normalizeTimes(times []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range times {
		if min, ok := parseHHMM(t); ok {
			norm := formatHHMM(min)
			if !seen[norm] {
				seen[norm] = true
				out = append(out, norm)
			}
		}
	}
	return out
}

func formatHHMM(min int) string {
	return pad2(min/60) + ":" + pad2(min%60)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func reminderMode(m string) string {
	if m == "once" {
		return "once"
	}
	return "daily"
}

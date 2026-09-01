package chat

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// --- Automations ---

func (d *DB) listAutomations(ctx context.Context, app string, enabledOnly bool) ([]Automation, error) {
	params := []any{}
	clauses := ""
	if app != "" {
		params = append(params, normalizeApp(app))
		clauses += " and (app_name is null or app_name = $1)"
	}
	if enabledOnly {
		clauses += " and enabled = true"
	}
	where := ""
	if clauses != "" {
		where = "where 1=1" + clauses
	}
	rows, err := d.pool.Query(ctx, `
		select id, title, patterns, response, app_name, enabled, send_payment_qr, min_score, attachment, created_at, updated_at
		from chat_automations `+where+` order by updated_at desc, id desc`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Automation{}
	for rows.Next() {
		var a Automation
		var attachRaw []byte
		if err := rows.Scan(&a.ID, &a.Title, &a.Patterns, &a.Response, &a.AppName, &a.Enabled, &a.SendPaymentQR, &a.MinScore, &attachRaw, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		if len(attachRaw) > 0 {
			_ = json.Unmarshal(attachRaw, &a.Attachment)
		}
		if a.Patterns == nil {
			a.Patterns = []string{}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) saveAutomation(ctx context.Context, id *int64, title string, patterns []string, response string, app *string, enabled, sendPaymentQR bool, minScore float64, attachment *AutomationAttachment) (*Automation, error) {
	var appName *string
	if app != nil && *app != "" {
		n := normalizeApp(*app)
		appName = &n
	}
	if patterns == nil {
		patterns = []string{}
	}
	// attachVal = NULL si no hay media; jsonb con los metadatos si la hay.
	var attachVal any
	if attachment != nil {
		b, err := json.Marshal(attachment)
		if err != nil {
			return nil, err
		}
		attachVal = b
	}
	var a Automation
	scan := func(row pgx.Row) error {
		var attachRaw []byte
		if err := row.Scan(&a.ID, &a.Title, &a.Patterns, &a.Response, &a.AppName, &a.Enabled, &a.SendPaymentQR, &a.MinScore, &attachRaw, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return err
		}
		if len(attachRaw) > 0 {
			_ = json.Unmarshal(attachRaw, &a.Attachment)
		} else {
			a.Attachment = nil
		}
		return nil
	}
	if id != nil && *id > 0 {
		row := d.pool.QueryRow(ctx, `
			update chat_automations set title=$2, patterns=$3, response=$4, app_name=$5, enabled=$6, send_payment_qr=$7, min_score=$8, attachment=$9, updated_at=now()
			where id=$1 returning id, title, patterns, response, app_name, enabled, send_payment_qr, min_score, attachment, created_at, updated_at`,
			*id, title, patterns, response, appName, enabled, sendPaymentQR, minScore, attachVal)
		if err := scan(row); err != nil {
			if err == pgx.ErrNoRows {
				return nil, nil
			}
			return nil, err
		}
		return &a, nil
	}
	row := d.pool.QueryRow(ctx, `
		insert into chat_automations (title, patterns, response, app_name, enabled, send_payment_qr, min_score, attachment)
		values ($1,$2,$3,$4,$5,$6,$7,$8)
		returning id, title, patterns, response, app_name, enabled, send_payment_qr, min_score, attachment, created_at, updated_at`,
		title, patterns, response, appName, enabled, sendPaymentQR, minScore, attachVal)
	if err := scan(row); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) deleteAutomation(ctx context.Context, id int64) error {
	_, err := d.pool.Exec(ctx, `delete from chat_automations where id = $1`, id)
	return err
}

// getAutomationAttachment devuelve el media (jsonb) de una automatización, o nil.
func (d *DB) getAutomationAttachment(ctx context.Context, id int64) (*AutomationAttachment, error) {
	var raw []byte
	err := d.pool.QueryRow(ctx, `select attachment from chat_automations where id = $1`, id).Scan(&raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var a AutomationAttachment
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// --- Admin notification settings ---

type notifSetting struct {
	AppName string `json:"app_name"`
	Email   string `json:"email"`
}

func (d *DB) listAdminNotificationSettings(ctx context.Context) ([]notifSetting, error) {
	rows, err := d.pool.Query(ctx, `select app_name, email from chat_admin_notification_settings order by app_name asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notifSetting{}
	for rows.Next() {
		var s notifSetting
		if err := rows.Scan(&s.AppName, &s.Email); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) getAdminNotificationEmail(ctx context.Context, app, fallback string) (string, error) {
	cleanApp := normalizeApp(app)
	var email string
	err := d.pool.QueryRow(ctx, `
		select email from chat_admin_notification_settings where app_name in ($1, 'default')
		order by case when app_name = $1 then 0 else 1 end limit 1`, cleanApp).Scan(&email)
	if err == pgx.ErrNoRows || email == "" {
		return fallback, nil
	}
	return email, err
}

func (d *DB) saveAdminNotificationSettings(ctx context.Context, defaultEmail, bcpEmail, yapeEmail, interbankEmail string) ([]notifSetting, error) {
	items := [][2]string{
		{"default", defaultEmail}, {"bcp", bcpEmail}, {"yape", yapeEmail}, {"interbank", interbankEmail},
	}
	for _, it := range items {
		if lowerTrim(it[1]) == "" {
			continue
		}
		if _, err := d.pool.Exec(ctx, `
			insert into chat_admin_notification_settings (app_name, email, updated_at) values ($1,$2,now())
			on conflict (app_name) do update set email = excluded.email, updated_at = now()`,
			it[0], normalizeEmail(it[1])); err != nil {
			return nil, err
		}
	}
	return d.listAdminNotificationSettings(ctx)
}

func (d *DB) updateAdminPassword(ctx context.Context, adminID int64, currentPassword, newPassword string) (bool, error) {
	var id int64
	err := d.pool.QueryRow(ctx, `
		update chat_users set password_hash = crypt($3, gen_salt('bf')), updated_at = now()
		where id = $1 and role = 'admin' and password_hash = crypt($2, password_hash)
		returning id`, adminID, currentPassword, newPassword).Scan(&id)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

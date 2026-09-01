// Package chat porta el módulo de chat E2E de producción (chat/ en Node) a Go,
// usando EL MISMO esquema de tablas para preservar los datos al migrar.
package chat

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL replica chat/sql/init.sql + las migraciones runtime de chat/db.js
// (ensureChatRuntimeSchema). Todo es idempotente (IF NOT EXISTS / ADD COLUMN IF
// NOT EXISTS), por lo que aplicarlo sobre la Postgres de producción NO borra ni
// altera los datos existentes: solo crea lo que falte.
const schemaSQL = `
create extension if not exists pgcrypto;

create table if not exists chat_users (
  id bigserial primary key,
  app_name text not null,
  email text not null,
  email_hash text not null,
  firebase_user_id text,
  nombre text,
  apellidos text,
  telefono text,
  guest_number bigint unique,
  role text not null default 'user' check (role in ('user', 'admin')),
  password_hash text,
  public_key text,
  last_seen_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (app_name, email_hash)
);

create sequence if not exists chat_guest_number_seq;
alter table chat_users add column if not exists guest_number bigint;
create unique index if not exists idx_chat_users_guest_number on chat_users(guest_number) where guest_number is not null;

create table if not exists chat_conversations (
  id bigserial primary key,
  app_name text not null,
  user_id bigint not null references chat_users(id) on delete cascade,
  admin_id bigint not null references chat_users(id) on delete restrict,
  status text not null default 'open' check (status in ('open', 'closed', 'blocked')),
  last_user_presence_at timestamptz,
  last_admin_presence_at timestamptz,
  last_user_seen_message_id bigint,
  last_admin_seen_message_id bigint,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (app_name, user_id, admin_id)
);
alter table chat_conversations
  add column if not exists last_user_presence_at timestamptz,
  add column if not exists last_admin_presence_at timestamptz,
  add column if not exists last_user_seen_message_id bigint,
  add column if not exists last_admin_seen_message_id bigint;

create table if not exists chat_messages (
  id bigserial primary key,
  conversation_id bigint not null references chat_conversations(id) on delete cascade,
  sender_id bigint not null references chat_users(id) on delete cascade,
  reply_to_message_id bigint references chat_messages(id) on delete set null,
  kind text not null check (kind in ('text', 'image', 'audio', 'video')),
  body_ciphertext text not null default '',
  body_iv text not null default '',
  body_tag text not null default '',
  client_nonce text not null,
  spam_fingerprint text,
  spam_signature text,
  created_at timestamptz not null default now(),
  deleted_at timestamptz,
  unique (conversation_id, client_nonce)
);
alter table chat_messages
  add column if not exists reply_to_message_id bigint references chat_messages(id) on delete set null,
  add column if not exists spam_fingerprint text,
  add column if not exists spam_signature text;

create table if not exists chat_message_reactions (
  id bigserial primary key,
  message_id bigint not null references chat_messages(id) on delete cascade,
  user_id bigint not null references chat_users(id) on delete cascade,
  emoji text not null check (char_length(emoji) between 1 and 24),
  created_at timestamptz not null default now(),
  unique (message_id, user_id, emoji)
);

create table if not exists chat_attachments (
  id bigserial primary key,
  message_id bigint not null unique references chat_messages(id) on delete cascade,
  uploader_id bigint not null references chat_users(id) on delete cascade,
  storage_path text not null,
  original_name text,
  mime_type text not null,
  size_bytes bigint not null,
  sha256 text not null,
  storage_encoding text not null default 'identity' check (storage_encoding in ('identity', 'gzip')),
  width integer,
  height integer,
  duration_ms integer,
  created_at timestamptz not null default now()
);
alter table chat_attachments
  add column if not exists storage_encoding text not null default 'identity';
alter table chat_attachments drop constraint if exists chat_attachments_size_bytes_check;
alter table chat_attachments
  add constraint chat_attachments_size_bytes_check
  check (size_bytes > 0 and size_bytes <= 62914560);
-- Verdicto antimalware (ClamAV). El archivo se guarda igual; esto solo lo marca.
alter table chat_attachments
  add column if not exists scan_status text not null default 'unscanned';
alter table chat_attachments
  add column if not exists scan_signature text;

-- Analítica de malware: un registro por adjunto detectado como infectado.
create table if not exists chat_malware_events (
  id bigserial primary key,
  conversation_id bigint references chat_conversations(id) on delete set null,
  uploader_id bigint references chat_users(id) on delete set null,
  uploader_role text,
  kind text,
  mime_type text,
  signature text,
  sha256 text,
  created_at timestamptz not null default now()
);
create index if not exists idx_chat_malware_events_uploader on chat_malware_events(uploader_id);
create index if not exists idx_chat_malware_events_created on chat_malware_events(created_at desc);

-- Etiquetas de conversación (organización del lado operador, estilo WhatsApp
-- Business). Son globales (compartidas por los admins) y se asignan N-a-N.
create table if not exists chat_labels (
  id bigserial primary key,
  name text not null,
  color text not null default '#008069',
  notify boolean not null default false,
  created_at timestamptz not null default now(),
  unique (name)
);
alter table chat_labels add column if not exists notify boolean not null default false;
create table if not exists chat_conversation_labels (
  conversation_id bigint not null references chat_conversations(id) on delete cascade,
  label_id bigint not null references chat_labels(id) on delete cascade,
  created_at timestamptz not null default now(),
  primary key (conversation_id, label_id)
);
create index if not exists idx_chat_conversation_labels_label on chat_conversation_labels(label_id);

-- Recordatorios: campañas de notificación push que el admin programa desde el
-- panel. Se envían a los usuarios de las conversaciones con la etiqueta objetivo
-- (ej. "debe"), en un rango de fechas, una sola vez o en varias franjas diarias.
create table if not exists chat_reminders (
  id bigserial primary key,
  title text not null,
  body text not null,
  label_id bigint references chat_labels(id) on delete set null,
  app text,
  route text,                         -- deep-link al tocar la notificación
  start_date date not null,
  end_date date not null,
  mode text not null default 'daily' check (mode in ('once','daily')),
  times jsonb not null default '[]',  -- ["08:00","12:00",...] (hora America/Lima)
  active boolean not null default true,
  created_at timestamptz not null default now()
);
-- Constancia de cada franja enviada, para no duplicar envíos.
create table if not exists chat_reminder_sends (
  reminder_id bigint not null references chat_reminders(id) on delete cascade,
  slot_key text not null,             -- "YYYY-MM-DD HH:MM"
  recipients int not null default 0,
  sent_at timestamptz not null default now(),
  primary key (reminder_id, slot_key)
);

create table if not exists chat_login_attempts (
  id bigserial primary key,
  app_name text not null,
  email_hash text not null,
  ip inet,
  success boolean not null,
  created_at timestamptz not null default now()
);

create table if not exists chat_sessions (
  jti uuid primary key,
  user_id bigint not null references chat_users(id) on delete cascade,
  role text not null check (role in ('user', 'admin')),
  app_name text,
  conversation_id bigint references chat_conversations(id) on delete cascade,
  email text,
  user_agent_hash text not null,
  ip_hash text not null,
  ip_address text,
  device_id text,
  revoked_at timestamptz,
  expires_at timestamptz not null,
  last_seen_at timestamptz,
  created_at timestamptz not null default now()
);
alter table chat_sessions add column if not exists ip_address text;
alter table chat_sessions add column if not exists device_id text;

create table if not exists chat_ws_tickets (
  ticket_hash text primary key,
  session_jti uuid not null references chat_sessions(jti) on delete cascade,
  consumed_at timestamptz,
  expires_at timestamptz not null,
  created_at timestamptz not null default now()
);

create table if not exists chat_automations (
  id bigserial primary key,
  title text not null,
  patterns text[] not null default '{}',
  response text not null,
  app_name text,
  enabled boolean not null default true,
  send_payment_qr boolean not null default false,
  min_score numeric not null default 0.45,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
-- Media opcional (imagen/video) que la respuesta automática envía junto al texto.
-- Guarda los metadatos del archivo ya preparado en disco (ruta, mime, encoding…).
alter table chat_automations add column if not exists attachment jsonb;
alter table chat_automations add column if not exists send_payment_qr boolean not null default false;

create table if not exists chat_notification_dedupe (
  dedupe_key text primary key,
  created_at timestamptz not null default now()
);

create table if not exists chat_admin_notification_settings (
  app_name text primary key,
  email text not null,
  updated_at timestamptz not null default now()
);

create table if not exists chat_automation_dedupe (
  conversation_id bigint not null references chat_conversations(id) on delete cascade,
  automation_id bigint not null references chat_automations(id) on delete cascade,
  question_hash text not null,
  last_sent_at timestamptz not null default now(),
  primary key (conversation_id, automation_id, question_hash)
);

-- La lista del panel ordena por updated_at desc: sin estos índices cada página
-- ordenaba la tabla entera de conversaciones. El id desagrupa los empates, que
-- con paginación por offset provocaban filas repetidas o saltadas entre páginas.
create index if not exists idx_chat_conversations_updated on chat_conversations(updated_at desc, id desc);
create index if not exists idx_chat_conversations_app_updated on chat_conversations(app_name, updated_at desc, id desc);
create index if not exists idx_chat_messages_conversation_id_id on chat_messages(conversation_id, id);
create index if not exists idx_chat_messages_reply_to_message_id on chat_messages(reply_to_message_id);
create index if not exists idx_chat_messages_spam_lookup on chat_messages(conversation_id, sender_id, id desc);
create index if not exists idx_chat_messages_spam_fingerprint on chat_messages(conversation_id, sender_id, spam_fingerprint);
create index if not exists idx_chat_message_reactions_message_id on chat_message_reactions(message_id);
create index if not exists idx_chat_attachments_uploader_id on chat_attachments(uploader_id);
`

// mergeQuickRepliesSQL fusiona las "respuestas rápidas" en la tabla única
// chat_automations (una respuesta rápida = una automatización cuyo patrón es su
// propio título), y luego ELIMINA las tablas viejas. Es idempotente: si las
// tablas ya no existen, no hace nada. NO se pierde ninguna respuesta guardada.
const mergeQuickRepliesSQL = `
do $$
begin
  if to_regclass('public.chat_quick_replies') is not null then
    insert into chat_automations (title, patterns, response, app_name, enabled, send_payment_qr, min_score)
    select q.title, array[q.title], q.body, q.app_name, q.enabled, false, 0.45
    from chat_quick_replies q
    where not exists (
      select 1 from chat_automations a
      where a.title = q.title and coalesce(a.app_name,'') = coalesce(q.app_name,'')
    );
    drop table if exists chat_quick_reply_steps;
    drop table if exists chat_quick_replies;
  end if;
end $$;
`

// seedAdminSQL siembra el admin del chat SOLO si no existe (on conflict do
// nothing, a diferencia del init.sql original que reseteaba el password en cada
// arranque). Así no se pisa una contraseña ya cambiada por el usuario.
const seedAdminSQL = `
insert into chat_users (app_name, email, email_hash, nombre, role, password_hash)
values (
  'admin',
  $1,
  encode(digest(lower($1), 'sha256'), 'hex'),
  'Admin',
  'admin',
  crypt($2, gen_salt('bf'))
)
on conflict (app_name, email_hash) do nothing;
`

// seedAutomationSQL replica el seed condicional de chat/db.js (automatización
// "Plan Medium - QR de pago" para yape), solo si no existe.
const seedAutomationSQL = `
insert into chat_automations (title, patterns, response, app_name, enabled, send_payment_qr, min_score)
select $1, $2, $3, $4, true, true, $5
where not exists (
  select 1 from chat_automations where title = $1 and app_name = $4
);
`

// EnsureSchema aplica el esquema del chat de forma idempotente y siembra el admin
// y la automatización por defecto. NO destruye datos existentes.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool, adminEmail, adminPassword string) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, mergeQuickRepliesSQL); err != nil {
		return err
	}
	if adminEmail == "" {
		adminEmail = "admin@codexpe.com"
	}
	if adminPassword == "" {
		adminPassword = "CambiarEstePassword123!"
	}
	if _, err := pool.Exec(ctx, seedAdminSQL, adminEmail, adminPassword); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, seedAutomationSQL,
		"Plan Medium - QR de pago",
		[]string{
			"Plan Medium Yape Fake mi nombre mi correo el plan",
			"Plan Medium Yape Fake",
			"Hola equipo de Yape Fake Mi nombre es Mi correo El Plan Plan Medium",
		},
		"Por favor realiza el pago en el QR enviado. Cuando termines, envianos la captura para validar tu Plan Medium.",
		"yape",
		0.35,
	)
	return err
}

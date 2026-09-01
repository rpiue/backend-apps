// Package notify centraliza el envío de notificaciones (FCM) COMO FUNCIÓN DIRECTA.
//
// En el código JS original, el cron y darPlan hacían `axios.post(dominio/api/notify/...)`
// (una llamada HTTP a sí mismo). Aquí, en cambio, se invoca notify.Notifier.NotifyUser(...)
// directamente en memoria: sin salto de red, más rápido y sin depender del dominio público.
//
// La regla de negocio del correo se conserva: si la app NO es "yape", al identificador
// (email/topic) se le añade el sufijo "-{app}", tal como pediste.
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Message es el payload neutral que luego se traduce a FCM (android/apns/data).
// Refleja los campos que manejaba buildFcmMessageTarget en notifications.js.
type Message struct {
	Title       string
	Body        string
	Route       string
	NotifID     string
	ChannelID   string
	Tag         string
	CollapseKey string
	Mode        string // "new" | "update" | "group"
	HeadsUp     bool
	Group       string
	Color       string
	ImageURL    string
	DataOnly    bool
	ForceLocal  bool
}

// Transport es la abstracción del proveedor (FCM vía Firebase Admin SDK en fase 2).
// Mantenerlo como interfaz permite testear y cambiar de proveedor sin tocar la lógica.
type Transport interface {
	SendToTopic(ctx context.Context, topic string, msg Message) error
	SendToToken(ctx context.Context, token string, msg Message) error
	SubscribeTopic(ctx context.Context, tokens []string, topic string) error
	UnsubscribeTopic(ctx context.Context, tokens []string, topic string) error
}

type Notifier struct {
	t Transport
}

func New(t Transport) *Notifier { return &Notifier{t: t} }

// NotifyUser envía directamente al topic único del usuario. Reemplaza el
// axios.post(/api/notify/notify/user/topic) del JS por una llamada de función.
func (n *Notifier) NotifyUser(ctx context.Context, email, app string, msg Message) error {
	if msg.Mode == "" {
		msg.Mode = "new"
	}
	return n.t.SendToTopic(ctx, UserTopic(email, app), msg)
}

// NotifyTopic envía a un topic arbitrario (planes, difusión, etc.).
func (n *Notifier) NotifyTopic(ctx context.Context, topic string, msg Message) error {
	return n.t.SendToTopic(ctx, topic, msg)
}

// NotifyToken envía a un token FCM concreto.
func (n *Notifier) NotifyToken(ctx context.Context, token string, msg Message) error {
	return n.t.SendToToken(ctx, token, msg)
}

func (n *Notifier) Subscribe(ctx context.Context, tokens []string, topic string) error {
	return n.t.SubscribeTopic(ctx, tokens, topic)
}

func (n *Notifier) Unsubscribe(ctx context.Context, tokens []string, topic string) error {
	return n.t.UnsubscribeTopic(ctx, tokens, topic)
}

// ApplyAppSuffix aplica la regla: todo lo que NO sea de la app "yape" lleva "-{app}".
// Conserva la convención del sistema de correos/notificaciones original.
func ApplyAppSuffix(identifier, app string) string {
	a := strings.ToLower(strings.TrimSpace(app))
	if a == "" || a == "yape" {
		return identifier
	}
	return identifier + "-" + a
}

// UserTopic replica userTopic(email) de notifications.js:
// sha256(email en minúsculas) -> primeros 24 hex -> "user_<hash>".
// Aplica además el sufijo de app antes de hashear, para aislar por aplicación.
func UserTopic(email, app string) string {
	id := ApplyAppSuffix(strings.ToLower(strings.TrimSpace(email)), app)
	sum := sha256.Sum256([]byte(id))
	return "user_" + hex.EncodeToString(sum[:])[:24]
}

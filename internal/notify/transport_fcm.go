package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2/google"
)

// FCMTransport envía notificaciones reales por FCM HTTP v1 y gestiona la
// suscripción a topics vía Instance ID API. Reemplaza el LogTransport.
//
// Usa una service account (proyecto notifications-c98d3) para autenticarse.
// Ambas SAs del JS son del mismo proyecto, así que basta una para enviar y
// para suscribir/desuscribir tokens a topics.
type FCMTransport struct {
	projectID string
	client    *http.Client // http.Client autenticado (oauth2) para scope messaging
}

// NewFCMTransport carga la service account `name` (sin .json) desde credsDir.
func NewFCMTransport(ctx context.Context, credsDir, name string) (*FCMTransport, error) {
	data, err := os.ReadFile(filepath.Join(credsDir, name+".json"))
	if err != nil {
		return nil, err
	}
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, err
	}
	cfg, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return nil, err
	}
	c := cfg.Client(ctx)
	c.Timeout = 15 * time.Second
	return &FCMTransport{projectID: sa.ProjectID, client: c}, nil
}

func (t *FCMTransport) post(ctx context.Context, url string, body any, headers map[string]string) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fcm %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

func (t *FCMTransport) SendToTopic(ctx context.Context, topic string, msg Message) error {
	payload := buildFCMPayload(map[string]any{"topic": topic}, msg)
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", t.projectID)
	return t.post(ctx, url, payload, nil)
}

// SendToToken envía a un token concreto (lo usan los endpoints user/update y user/group).
func (t *FCMTransport) SendToToken(ctx context.Context, token string, msg Message) error {
	payload := buildFCMPayload(map[string]any{"token": token}, msg)
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", t.projectID)
	return t.post(ctx, url, payload, nil)
}

func (t *FCMTransport) SubscribeTopic(ctx context.Context, tokens []string, topic string) error {
	if len(tokens) == 0 {
		return nil
	}
	body := map[string]any{"to": "/topics/" + topic, "registration_tokens": tokens}
	return t.post(ctx, "https://iid.googleapis.com/iid/v1:batchAdd", body, map[string]string{"access_token_auth": "true"})
}

func (t *FCMTransport) UnsubscribeTopic(ctx context.Context, tokens []string, topic string) error {
	if len(tokens) == 0 {
		return nil
	}
	body := map[string]any{"to": "/topics/" + topic, "registration_tokens": tokens}
	return t.post(ctx, "https://iid.googleapis.com/iid/v1:batchRemove", body, map[string]string{"access_token_auth": "true"})
}

// Package ai habla con el proveedor de IA del chat. Soporta dos backends:
//   - "ollama": modelo LOCAL (Ollama, /api/chat).
//   - "nvidia" (u otro OpenAI-compatible): IA en la NUBE (NVIDIA free, /v1/chat/completions).
//
// El proveedor se elige por config (AI_PROVIDER), normalmente desde start.sh.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex/backend/internal/secnet"
)

type Client struct {
	provider string // "ollama" | "nvidia"
	baseURL  string
	apiKey   string
	model    string // modelo por defecto de este proveedor
	http     *http.Client
}

// Options configura el cliente. Provider vacío = "ollama".
type Options struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

func New(o Options) *Client {
	prov := strings.ToLower(strings.TrimSpace(o.Provider))
	if prov == "" {
		prov = "ollama"
	}
	return &Client{
		provider: prov,
		baseURL:  strings.TrimRight(strings.TrimSpace(o.BaseURL), "/"),
		apiKey:   strings.TrimSpace(o.APIKey),
		model:    strings.TrimSpace(o.Model),
		http:     secnet.Client(120 * time.Second),
	}
}

func (c *Client) Provider() string     { return c.provider }
func (c *Client) DefaultModel() string { return c.model }
func (c *Client) isOllama() bool       { return c.provider == "ollama" }

// Msg es un mensaje del chat (role: system|user|assistant).
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Chat envía la conversación al modelo y devuelve la respuesta.
func (c *Client) Chat(ctx context.Context, model string, msgs []Msg) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = c.model
	}
	if c.isOllama() {
		return c.chatOllama(ctx, model, msgs)
	}
	return c.chatOpenAI(ctx, model, msgs)
}

// chatOllama: API nativa de Ollama (modelo local).
func (c *Client) chatOllama(ctx context.Context, model string, msgs []Msg) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "messages": msgs, "stream": false,
		"options": map[string]any{"temperature": 0.4},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("ollama %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		Message Msg `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
}

// chatOpenAI: API OpenAI-compatible (NVIDIA free y similares).
func (c *Client) chatOpenAI(ctx context.Context, model string, msgs []Msg) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "messages": msgs, "stream": false, "temperature": 0.4,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s %d: %s", c.provider, resp.StatusCode, string(data))
	}
	var out struct {
		Choices []struct {
			Message Msg `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}

// Pull descarga/carga el modelo. Solo aplica a Ollama; en la nube es no-op.
func (c *Client) Pull(ctx context.Context, model string) error {
	if !c.isOllama() {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"name": model, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := secnet.Client(30 * time.Minute) // la descarga puede tardar
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ollama pull %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

// Ready indica si el proveedor está disponible. Ollama: responde /api/tags.
// Nube: listo si hay token configurado.
func (c *Client) Ready(ctx context.Context) bool {
	if !c.isOllama() {
		return c.apiKey != ""
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

// Package mercadopago es un cliente mínimo de la API de MercadoPago: crear
// preferencia de checkout y consultar un pago. Reemplaza los axios de pago.js.
package mercadopago

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
	token string
	http  *http.Client
}

func New(token string) *Client {
	return &Client{token: token, http: secnet.Client(20 * time.Second)}
}

type PreferenceInput struct {
	Email       string
	Nombre      string
	Monto       float64
	Descripcion string
	Plan        string
	App         string
	PlanN       int
	CompraID    string
	NotifyURL   string
}

// CrearPreferencia crea la preferencia y devuelve (initPoint, preferenceID).
func (c *Client) CrearPreferencia(ctx context.Context, in PreferenceInput) (string, string, error) {
	body := map[string]any{
		"payer": map[string]any{"last_name": in.Nombre, "email": in.Email},
		"items": []any{map[string]any{
			"title": orStr(in.Descripcion, "Producto o servicio"), "quantity": 1,
			"currency_id": "PEN", "unit_price": in.Monto,
		}},
		"payment_methods": map[string]any{
			"excluded_payment_types": []any{
				map[string]any{"id": "credit_card"},
				map[string]any{"id": "debit_card"},
			},
		},
		"metadata": map[string]any{
			"plan": orStr(in.Plan, "Desconocido"), "app": orStr(in.App, "Desconocido"),
			"monto": in.Monto, "compraId": in.CompraID, "planN": in.PlanN, "nombre": in.Nombre,
		},
		"notification_url":     in.NotifyURL,
		"external_reference":   in.Email,
		"statement_descriptor": "MERCADOPAGO",
	}
	var out struct {
		InitPoint string `json:"init_point"`
		ID        string `json:"id"`
	}
	if err := c.post(ctx, "https://api.mercadopago.com/checkout/preferences", body, &out); err != nil {
		return "", "", err
	}
	return out.InitPoint, out.ID, nil
}

// PagoEfectivoInput crea un pago en EFECTIVO (CIP de PagoEfectivo) directamente
// por la API de Pagos, sin abrir navegador. MercadoPago devuelve el código en la
// respuesta, así que reemplaza al scraper de Playwright (que se rompía por la
// detección de bots y los cambios de la página de checkout).
type PagoEfectivoInput struct {
	Email       string
	Nombre      string
	Monto       float64
	Descripcion string
	Plan        string
	App         string
	PlanN       int
	CompraID    string
	NotifyURL   string
}

// PagoEfectivoResult es lo que necesita /sara: el código a dictar y el ticket.
type PagoEfectivoResult struct {
	ID     string
	Status string
	Codigo string // el CIP que ve el cliente: transaction_details.payment_method_reference_id
	Ticket string // transaction_details.external_resource_url (link de MercadoPago)
}

// CrearPagoEfectivo hace POST /v1/payments con payment_method_id=pagoefectivo_atm.
// La metadata (plan/app/planN/compraId/nombre) viaja en el pago para que el webhook
// la reciba tal cual al aprobarse, igual que hacía la preferencia.
func (c *Client) CrearPagoEfectivo(ctx context.Context, in PagoEfectivoInput) (*PagoEfectivoResult, error) {
	nombre := strings.TrimSpace(in.Nombre)
	first, last := nombre, ""
	if i := strings.IndexByte(nombre, ' '); i > 0 {
		first, last = nombre[:i], strings.TrimSpace(nombre[i+1:])
	}
	if first == "" {
		first = "Cliente"
	}
	body := map[string]any{
		"transaction_amount": in.Monto,
		"description":        orStr(in.Descripcion, "Plan"),
		"payment_method_id":  "pagoefectivo_atm",
		"payer":              map[string]any{"email": in.Email, "first_name": first, "last_name": last},
		"external_reference": in.Email,
		"notification_url":   in.NotifyURL,
		"metadata": map[string]any{
			"plan": orStr(in.Plan, "Desconocido"), "app": orStr(in.App, "Desconocido"),
			"monto": in.Monto, "compraId": in.CompraID, "planN": in.PlanN, "nombre": in.Nombre,
		},
	}
	var out struct {
		ID                 json.Number `json:"id"` // entero grande: json.Number evita la notación científica de float64
		Status             string      `json:"status"`
		TransactionDetails struct {
			// El CIP que el cliente dicta en BCP/agentes (9 díg, ej. 398758867). Es
			// el que se muestra en el ticket como "Código de pago (CIP)". OJO: NO es
			// `verification_code` (11 díg), que es una referencia interna distinta.
			PaymentMethodReferenceID string `json:"payment_method_reference_id"`
			ExternalResourceURL      string `json:"external_resource_url"`
		} `json:"transaction_details"`
	}
	// X-Idempotency-Key (compraId): evita pagos duplicados ante reintentos.
	if err := c.postIdem(ctx, "https://api.mercadopago.com/v1/payments", in.CompraID, body, &out); err != nil {
		return nil, err
	}
	return &PagoEfectivoResult{
		ID:     out.ID.String(),
		Status: out.Status,
		Codigo: out.TransactionDetails.PaymentMethodReferenceID,
		Ticket: out.TransactionDetails.ExternalResourceURL,
	}, nil
}

// Payment es la parte del pago que usamos del webhook.
type Payment struct {
	Status string `json:"status"`
	ID     any    `json:"id"`
	Payer  struct {
		Email string `json:"email"`
	} `json:"payer"`
	Metadata map[string]any `json:"metadata"`
}

// GetPayment consulta un pago por id.
func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.mercadopago.com/v1/payments/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mp payment %d: %s", resp.StatusCode, string(data))
	}
	var p Payment
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) post(ctx context.Context, url string, body, out any) error {
	return c.postIdem(ctx, url, "", body, out)
}

// postIdem es como post pero añade X-Idempotency-Key si idemKey != "" (requerido
// por /v1/payments para no duplicar pagos ante reintentos).
func (c *Client) postIdem(ctx context.Context, url, idemKey string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("X-Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mp %d: %s", resp.StatusCode, string(data))
	}
	return json.Unmarshal(data, out)
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

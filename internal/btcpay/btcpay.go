// Package btcpay replica btcPagos.js: crea invoices en BTCPay Server y scrapea
// la página pública del invoice para extraer address/monto/lightning (BIP21).
package btcpay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"codex/backend/internal/secnet"
)

// Config son las credenciales del store de BTCPay (desde variables de entorno).
type Config struct {
	URL                  string
	StoreID              string
	APIKey               string
	CFAccessClientID     string
	CFAccessClientSecret string
}

// Client habla con la API de BTCPay.
type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client {
	if cfg.URL == "" {
		cfg.URL = "https://pay.codexpe.com"
	}
	return &Client{cfg: cfg, http: secnet.Client(20 * time.Second)}
}

// Enabled indica si hay credenciales suficientes para operar.
func (c *Client) Enabled() bool {
	return c.cfg.StoreID != "" && c.cfg.APIKey != ""
}

// CrearPagoInput son los datos del invoice a crear.
type CrearPagoInput struct {
	Precio float64
	Email  string
	Plan   string
	App    string
	PlanN  int
}

// PagoResult replica el objeto que devolvía crearPago().
type PagoResult struct {
	BTC    bool   `json:"btc"`
	OK     bool   `json:"ok"`
	Wallet string `json:"wallet,omitempty"`
	Monto  string `json:"monto,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CrearPago crea un invoice y devuelve wallet (lightning o address) + monto en BTC.
func (c *Client) CrearPago(ctx context.Context, in CrearPagoInput) PagoResult {
	app := in.App
	if app == "" {
		app = "yape"
	}
	plan := in.Plan
	if plan == "" {
		plan = "Desconocido"
	}
	body := map[string]any{
		"amount":   in.Precio,
		"currency": "USD",
		"metadata": map[string]any{
			"plan":  plan,
			"app":   app,
			"monto": in.Precio,
			"planN": in.PlanN,
			"email": orDef(in.Email, "Sin nombre"),
		},
	}

	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/stores/%s/invoices", c.cfg.URL, c.cfg.StoreID), bytes.NewReader(raw))
	if err != nil {
		return PagoResult{BTC: true, OK: false, Error: "No se pudo crear la orden"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+c.cfg.APIKey)
	if c.cfg.CFAccessClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.cfg.CFAccessClientID)
		req.Header.Set("CF-Access-Client-Secret", c.cfg.CFAccessClientSecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return PagoResult{BTC: true, OK: false, Error: "No se pudo crear la orden"}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		return PagoResult{BTC: true, OK: false, Error: "No se pudo crear la orden"}
	}
	var inv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &inv); err != nil || inv.ID == "" {
		return PagoResult{BTC: true, OK: false, Error: "No se pudo crear la orden"}
	}

	pago, err := c.obtenerAddressYMonto(ctx, inv.ID)
	if err != nil || !pago.ok {
		return PagoResult{BTC: true, OK: false, Error: "No se pudo crear la orden"}
	}
	wallet := pago.address
	if pago.lightning != "" {
		wallet = pago.lightning
	}
	return PagoResult{BTC: true, OK: true, Wallet: wallet, Monto: pago.amountBTC}
}

type invoiceData struct {
	ok        bool
	status    string
	address   string
	amountBTC string
	lightning string
	qr        string
}

var srvModelRe = regexp.MustCompile(`(?s)const\s+initialSrvModel\s*=\s*(\{.*?\});`)

// obtenerAddressYMonto replica obtenerAddressYMonto(): scrapea /i/{id} y parsea
// el objeto initialSrvModel embebido en el HTML.
func (c *Client) obtenerAddressYMonto(ctx context.Context, invoiceID string) (invoiceData, error) {
	u := fmt.Sprintf("%s/i/%s", c.cfg.URL, invoiceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return invoiceData{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "text/html")
	resp, err := c.http.Do(req)
	if err != nil {
		return invoiceData{}, err
	}
	defer resp.Body.Close()
	html, _ := io.ReadAll(resp.Body)

	m := srvModelRe.FindSubmatch(html)
	if m == nil {
		return invoiceData{ok: false}, nil
	}
	var model struct {
		Status            string `json:"status"`
		Address           string `json:"address"`
		Due               any    `json:"due"`
		InvoiceBitcoinURL string `json:"invoiceBitcoinUrl"`
	}
	if err := json.Unmarshal(m[1], &model); err != nil {
		return invoiceData{ok: false}, nil
	}
	if model.Status != "New" && model.Status != "Processing" {
		return invoiceData{ok: false, status: model.Status}, nil
	}
	due := fmt.Sprintf("%v", model.Due)
	if model.Address == "" || model.Due == nil || due == "" {
		return invoiceData{ok: false, status: model.Status}, nil
	}
	out := invoiceData{ok: true, status: model.Status, address: model.Address, amountBTC: due}
	if bolt11, bip21 := parseBip21(model.InvoiceBitcoinURL); bolt11 != "" || bip21 != "" {
		out.lightning = bolt11
		out.qr = bip21
	}
	return out, nil
}

// parseBip21 replica parseBip21(): extrae bolt11 (lightning) y el bip21 normalizado.
func parseBip21(s string) (bolt11, bip21 string) {
	if s == "" {
		return "", ""
	}
	bip21 = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, `&`, "&"), "&", "&"))
	clean := bip21
	if strings.HasPrefix(strings.ToLower(clean), "bitcoin:") {
		clean = clean[len("bitcoin:"):]
	}
	parts := strings.SplitN(clean, "?", 2)
	if len(parts) == 2 {
		if q, err := url.ParseQuery(parts[1]); err == nil {
			bolt11 = q.Get("lightning")
		}
	}
	return bolt11, bip21
}

func orDef(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

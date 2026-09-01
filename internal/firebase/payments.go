package firebase

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Compra (en Firestore pagosApp/{email}/compras/{id}) para el flujo de /sara.
type Compra struct {
	ID         string
	Titulo     string
	Plan       string
	Status     string
	CodigoPago string
	Link       string
	App        string
	Monto      int
}

// CrearCompra replica crearCompra(email, compra) de datos-pagos.js: crea (o
// actualiza codigoPago/status) la compra bajo pagosApp/{email}/compras/{id}.
// Usa una transacción para leer usuario+compra antes de escribir.
func (c *Client) CrearCompra(ctx context.Context, email string, compra Compra) error {
	p := c.Registry.ControlPagos()
	_email := strings.ToLower(strings.TrimSpace(email))
	if _email == "" {
		return fmt.Errorf("Email requerido")
	}
	if compra.Plan == "" || compra.Titulo == "" {
		return fmt.Errorf("plan y titulo requeridos")
	}
	userPath := "pagosApp/" + _email
	compraPath := userPath + "/compras/" + compra.ID

	now := time.Now()
	cipExpira := now.Add(48 * time.Hour)

	txn, err := c.BeginTransaction(ctx, p)
	if err != nil {
		return err
	}
	userDoc, userExists, err := c.GetDocTx(ctx, p, userPath, txn)
	_ = userDoc
	if err != nil {
		return err
	}
	compraDoc, compraExists, err := c.GetDocTx(ctx, p, compraPath, txn)
	if err != nil {
		return err
	}

	var writes []Write
	if !userExists {
		writes = append(writes, Write{Path: userPath, Fields: map[string]any{
			"email": _email, "createdAt": Timestamp{Time: now},
		}})
	}
	if !compraExists {
		writes = append(writes, Write{Path: compraPath, Fields: map[string]any{
			"titulo":       compra.Titulo,
			"plan":         compra.Plan,
			"codigoPago":   nilIfEmpty(compra.CodigoPago),
			"status":       defaultStr(compra.Status, "pendiente"),
			"fechaInicial": Timestamp{Time: now},
			"cipExpira":    Timestamp{Time: cipExpira},
			"link":         compra.Link,
			"createdAt":    Timestamp{Time: now},
			"updatedAt":    Timestamp{Time: now},
		}})
	} else {
		upd := map[string]any{"updatedAt": Timestamp{Time: now}}
		mask := []string{"updatedAt"}
		if strings.TrimSpace(compra.CodigoPago) != "" {
			upd["codigoPago"] = strings.TrimSpace(compra.CodigoPago)
			mask = append(mask, "codigoPago")
		}
		if strings.TrimSpace(compra.Status) != "" {
			upd["status"] = strings.TrimSpace(compra.Status)
			mask = append(mask, "status")
		}
		_ = compraDoc
		writes = append(writes, Write{Path: compraPath, Fields: upd, UpdateMask: mask})
	}

	return c.Commit(ctx, p, writes, txn)
}

// GetCipPendienteVigente replica getCipPendienteVigente(email, plan): busca una
// compra pendiente/creado del mismo plan con CIP aún vigente (cipExpira >= ahora).
func (c *Client) GetCipPendienteVigente(ctx context.Context, email, plan string) (*Compra, map[string]any, error) {
	p := c.Registry.ControlPagos()
	_email := strings.ToLower(strings.TrimSpace(email))
	userPath := "pagosApp/" + _email

	_, exists, err := c.GetDoc(ctx, p, userPath)
	if err != nil || !exists {
		return nil, nil, err
	}

	docs, err := c.Query(ctx, p, userPath, "compras", []Filter{
		{Field: "plan", Op: "EQUAL", Value: plan},
		{Field: "status", Op: "IN", Value: []any{"pendiente", "creado"}},
	}, 25)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	type cand struct {
		doc   Doc
		expMs int64
	}
	var cands []cand
	for _, d := range docs {
		if exp, ok := asTime(d.Data["cipExpira"]); ok && !exp.Before(now) {
			cands = append(cands, cand{doc: d, expMs: exp.UnixMilli()})
		}
	}
	if len(cands) == 0 {
		return nil, nil, nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].expMs > cands[j].expMs })
	best := cands[0].doc

	out := &Compra{ID: best.ID}
	if s, ok := best.Data["codigoPago"].(string); ok {
		out.CodigoPago = s
	}
	if s, ok := best.Data["plan"].(string); ok {
		out.Plan = s
	}
	if s, ok := best.Data["titulo"].(string); ok {
		out.Titulo = s
	}
	if s, ok := best.Data["link"].(string); ok {
		out.Link = s
	}
	if s, ok := best.Data["status"].(string); ok {
		out.Status = s
	}
	return out, best.Data, nil
}

// ActualizarEstadoCompra replica actualizarEstadoCompraPorId: marca la compra
// como pagada (si no estaba ya finalizada), con extras (fechaPago, mpPaymentId...).
func (c *Client) ActualizarEstadoCompra(ctx context.Context, email, compraID, nuevoStatus string, extra map[string]any) error {
	p := c.Registry.ControlPagos()
	_email := strings.ToLower(strings.TrimSpace(email))
	userPath := "pagosApp/" + _email
	compraPath := userPath + "/compras/" + compraID

	txn, err := c.BeginTransaction(ctx, p)
	if err != nil {
		return err
	}
	if _, ok, err := c.GetDocTx(ctx, p, userPath, txn); err != nil || !ok {
		return err
	}
	fresh, ok, err := c.GetDocTx(ctx, p, compraPath, txn)
	if err != nil || !ok {
		return err
	}
	statusActual := strings.ToLower(fmt.Sprintf("%v", fresh.Data["status"]))
	switch statusActual {
	case "pagado", "aprobado", "cancelado":
		// ya finalizada: no reescribir
		return nil
	}
	upd := map[string]any{"status": nuevoStatus, "updatedAt": Timestamp{Time: time.Now()}}
	mask := []string{"status", "updatedAt"}
	for k, v := range extra {
		upd[k] = v
		mask = append(mask, k)
	}
	return c.Commit(ctx, p, []Write{{Path: compraPath, Fields: upd, UpdateMask: mask}}, txn)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

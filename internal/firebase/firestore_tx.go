package firebase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// Filter es un filtro de consulta (fieldFilter). Op: "EQUAL", "IN", etc.
type Filter struct {
	Field string
	Op    string
	Value any
}

// Query ejecuta una consulta estructurada en una colección (top-level o subcolección).
// parentPath vacío = raíz; p.ej. parentPath="codigosApp/email" + collectionID="codigos".
// Varios filtros se combinan con AND (compositeFilter).
func (c *Client) Query(ctx context.Context, p Project, parentPath, collectionID string, filters []Filter, limitN int) ([]Doc, error) {
	sq := map[string]any{"from": []any{map[string]any{"collectionId": collectionID}}}
	if len(filters) == 1 {
		sq["where"] = fieldFilter(filters[0])
	} else if len(filters) > 1 {
		fs := make([]any, 0, len(filters))
		for _, f := range filters {
			fs = append(fs, fieldFilter(f))
		}
		sq["where"] = map[string]any{"compositeFilter": map[string]any{"op": "AND", "filters": fs}}
	}
	if limitN > 0 {
		sq["limit"] = limitN
	}

	base := p.base()
	if parentPath != "" {
		base += "/" + parentPath
	}
	return c.runQueryAt(ctx, p, base+":runQuery?key="+p.APIKey, sq)
}

func fieldFilter(f Filter) map[string]any {
	return map[string]any{
		"fieldFilter": map[string]any{
			"field": map[string]any{"fieldPath": f.Field},
			"op":    f.Op,
			"value": encodeValue(f.Value),
		},
	}
}

func (c *Client) runQueryAt(ctx context.Context, p Project, url string, sq map[string]any) ([]Doc, error) {
	b, _ := json.Marshal(map[string]any{"structuredQuery": sq})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := p.bearerToken(ctx); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("firestore query %d: %s", resp.StatusCode, string(data))
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	out := []Doc{}
	for _, row := range rows {
		if doc, ok := row["document"].(map[string]any); ok {
			out = append(out, docFromRaw(doc))
		}
	}
	return out, nil
}

// --- Transacciones y escrituras por lote (commit) ---

// Write describe una escritura: set/update (con Fields) o delete.
// UpdateMask nil = reemplazo total del doc; con máscara = actualización parcial.
type Write struct {
	Path       string
	Fields     map[string]any
	UpdateMask []string
	Delete     bool
}

func (c *Client) writeJSON(p Project, w Write) map[string]any {
	if w.Delete {
		return map[string]any{"delete": p.fullName(w.Path)}
	}
	out := map[string]any{
		"update": map[string]any{
			"name":   p.fullName(w.Path),
			"fields": encodeFields(w.Fields),
		},
	}
	if w.UpdateMask != nil {
		out["updateMask"] = map[string]any{"fieldPaths": w.UpdateMask}
	}
	return out
}

// Commit aplica un conjunto de escrituras de forma atómica (writeBatch / commit
// de transacción). txn vacío = batch sin transacción.
func (c *Client) Commit(ctx context.Context, p Project, writes []Write, txn string) error {
	ws := make([]any, 0, len(writes))
	for _, w := range writes {
		ws = append(ws, c.writeJSON(p, w))
	}
	body := map[string]any{"writes": ws}
	if txn != "" {
		body["transaction"] = txn
	}
	log.Printf("[firestore] Commit project=%s writes=%d txn=%q", p.ID, len(writes), txn)
	_, status, err := c.do(ctx, p, http.MethodPost, p.base()+":commit?key="+p.APIKey, body)
	if status == http.StatusConflict {
		log.Printf("[firestore] Commit conflict: project=%s txn=%q", p.ID, txn)
		return fmt.Errorf("transacción abortada (conflicto)")
	}
	if err != nil {
		log.Printf("[firestore] Commit error: project=%s txn=%q err=%v", p.ID, txn, err)
		return err
	}
	log.Printf("[firestore] Commit ok: project=%s writes=%d txn=%q", p.ID, len(writes), txn)
	return nil
}

// BeginTransaction inicia una transacción read-write y devuelve su id.
func (c *Client) BeginTransaction(ctx context.Context, p Project) (string, error) {
	body := map[string]any{"options": map[string]any{"readWrite": map[string]any{}}}
	log.Printf("[firestore] BeginTransaction project=%s", p.ID)
	out, _, err := c.do(ctx, p, http.MethodPost, p.txEndpoint("beginTransaction"), body)
	if err != nil {
		log.Printf("[firestore] BeginTransaction failed: project=%s err=%v", p.ID, err)
		return "", err
	}
	if t, ok := out["transaction"].(string); ok {
		log.Printf("[firestore] BeginTransaction ok: project=%s txn=%q", p.ID, t)
		return t, nil
	}
	log.Printf("[firestore] BeginTransaction invalid response: project=%s out=%v", p.ID, out)
	return "", fmt.Errorf("beginTransaction: sin id")
}

// Rollback cancela una transacción abierta y LIBERA los bloqueos que mantiene
// sobre los documentos leídos.
//
// Hace falta porque una transacción que se abandona sin cerrar no desaparece:
// Firestore la mantiene con sus bloqueos hasta que expira sola (unos 60 s).
// Cada intento fallido que salga por un camino que no llega al Commit deja una
// colgando, y suficientes intentos seguidos —justo lo que hace quien prueba
// códigos en serie— acaban trabando los documentos para todo el mundo.
//
// No devuelve error a propósito: se llama desde defer, donde no hay nada que
// hacer con el fallo salvo registrarlo, y no debe tapar el error original.
func (c *Client) Rollback(ctx context.Context, p Project, txn string) {
	if txn == "" {
		return
	}
	log.Printf("[firestore] Rollback project=%s txn=%q", p.ID, txn)
	if _, _, err := c.do(ctx, p, http.MethodPost, p.txEndpoint("rollback"),
		map[string]any{"transaction": txn}); err != nil {
		log.Printf("[firestore] rollback failed: project=%s txn=%q err=%v", p.ID, txn, err)
	}
}

// GetDocTx lee un documento dentro de una transacción.
func (c *Client) GetDocTx(ctx context.Context, p Project, path, txn string) (Doc, bool, error) {
	u := p.base() + "/" + path + "?key=" + p.APIKey
	if txn != "" {
		u += "&transaction=" + txn
	}
	log.Printf("[firestore] GetDocTx project=%s path=%s txn=%q", p.ID, path, txn)
	out, status, err := c.do(ctx, p, http.MethodGet, u, nil)
	if status == http.StatusNotFound {
		log.Printf("[firestore] GetDocTx not found: project=%s path=%s txn=%q", p.ID, path, txn)
		return Doc{}, false, nil
	}
	if err != nil {
		log.Printf("[firestore] GetDocTx error: project=%s path=%s txn=%q err=%v", p.ID, path, txn, err)
		return Doc{}, false, err
	}
	return docFromRaw(out), true, nil
}

// Package firebase es un cliente de la API REST de Firestore. Replica el acceso
// que hacía el SDK web (firebase/firestore/lite) con apiKey, sin necesitar
// service accounts. Cubre: getDoc, query(where ==), updateDoc, addDoc, setDoc.
package firebase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"codex/backend/internal/secnet"

	"golang.org/x/oauth2/google"
)

type Client struct {
	http     *http.Client
	Registry *Registry
}

func New() *Client {
	return &Client{
		http:     secnet.Client(20 * time.Second),
		Registry: NewRegistry(),
	}
}

// Doc es un documento decodificado: ID + datos nativos.
type Doc struct {
	ID   string         `json:"id"`
	Data map[string]any `json:"-"`
}

func (p Project) dbRoot() string {
	return "https://firestore.googleapis.com/v1/projects/" + p.ID + "/databases/(default)"
}

func (p Project) base() string {
	return p.dbRoot() + "/documents"
}

func (p Project) txEndpoint(action string) string {
	return p.base() + ":" + action + "?key=" + p.APIKey
}

func (p Project) bearerToken(ctx context.Context) string {
	if p.CredentialsFile != "" {
		b, err := os.ReadFile(p.CredentialsFile)
		if err == nil {
			creds, err := google.CredentialsFromJSON(ctx, b, "https://www.googleapis.com/auth/cloud-platform")
			if err == nil && creds != nil && creds.TokenSource != nil {
				tok, err := creds.TokenSource.Token()
				if err == nil && tok != nil && tok.AccessToken != "" {
					return "Bearer " + tok.AccessToken
				}
			}
		}
	}
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return ""
	}
	tok, err := ts.Token()
	if err != nil || tok == nil || tok.AccessToken == "" {
		return ""
	}
	return "Bearer " + tok.AccessToken
}

// fullName arma el nombre RELATIVO del recurso de un documento, que es lo que la
// API :commit de Firestore espera en `writes[].update.name` / `.delete`. Debe
// empezar en "projects/…"; NO lleva el prefijo host+versión (https://…/v1/): con
// la URL completa, Firestore responde 400 "lacks 'projects' at index 0" y las
// escrituras por lote (códigos grupales, consumirCodigo) fallaban.
func (p Project) fullName(path string) string {
	return "projects/" + p.ID + "/databases/(default)/documents/" + path
}

// idFromName extrae el ID (último segmento) del campo name de un documento.
func idFromName(name string) string {
	i := strings.LastIndex(name, "/documents/")
	if i >= 0 {
		name = name[i+len("/documents/"):]
	}
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func (c *Client) do(ctx context.Context, p Project, method, rawURL string, body any) (map[string]any, int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	log.Printf("[firestore] request method=%s target=%s bodyLen=%d", method, redactFirestoreURL(rawURL), bodyLen(body))
	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
	if err != nil {
		log.Printf("[firestore] request build error: method=%s url=%s err=%v", method, redactFirestoreURL(rawURL), err)
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := p.bearerToken(ctx); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[firestore] request failed: method=%s url=%s err=%v", method, redactFirestoreURL(rawURL), err)
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	if resp.StatusCode >= 400 {
		msg := resp.Status
		if e, ok := out["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
		}
		log.Printf("[firestore] http error: method=%s target=%s status=%d body=%s", method, redactFirestoreURL(rawURL), resp.StatusCode, truncateLog(string(data), 1200))
		return out, resp.StatusCode, fmt.Errorf("firestore %d: %s", resp.StatusCode, msg)
	}
	log.Printf("[firestore] ok: method=%s target=%s status=%d", method, redactFirestoreURL(rawURL), resp.StatusCode)
	return out, resp.StatusCode, nil
}

func redactFirestoreURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if u, err := url.Parse(rawURL); err == nil {
		q := u.Query()
		q.Del("key")
		u.RawQuery = q.Encode()
		return u.String()
	}
	return rawURL
}

func bodyLen(v any) int {
	if v == nil {
		return 0
	}
	b, _ := json.Marshal(v)
	return len(b)
}

func mustJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GetDoc lee un documento por path (p.ej. "usuarios/ID"). Devuelve found=false si no existe.
func (c *Client) GetDoc(ctx context.Context, p Project, path string) (Doc, bool, error) {
	u := p.base() + "/" + path + "?key=" + p.APIKey
	log.Printf("[firestore] GetDoc project=%s path=%s", p.ID, path)
	out, status, err := c.do(ctx, p, http.MethodGet, u, nil)
	if status == http.StatusNotFound {
		log.Printf("[firestore] GetDoc not found: project=%s path=%s", p.ID, path)
		return Doc{}, false, nil
	}
	if err != nil {
		log.Printf("[firestore] GetDoc error: project=%s path=%s err=%v", p.ID, path, err)
		return Doc{}, false, err
	}
	return docFromRaw(out), true, nil
}

func docFromRaw(out map[string]any) Doc {
	d := Doc{}
	if name, ok := out["name"].(string); ok {
		d.ID = idFromName(name)
	}
	if f, ok := out["fields"].(map[string]any); ok {
		d.Data = decodeFields(f)
	} else {
		d.Data = map[string]any{}
	}
	return d
}

// QueryEqual ejecuta una consulta `where(field == value)` sobre una colección.
// Replica query(collection, where(field,'==',value)) del SDK web.
func (c *Client) QueryEqual(ctx context.Context, p Project, collection, field string, value any, limitN int) ([]Doc, error) {
	sq := map[string]any{
		"from": []any{map[string]any{"collectionId": collection}},
		"where": map[string]any{
			"fieldFilter": map[string]any{
				"field": map[string]any{"fieldPath": field},
				"op":    "EQUAL",
				"value": encodeValue(value),
			},
		},
	}
	if limitN > 0 {
		sq["limit"] = limitN
	}
	return c.runQuery(ctx, p, sq)
}

// ListCollection devuelve todos los documentos de una colección (sin filtro).
func (c *Client) ListCollection(ctx context.Context, p Project, collection string) ([]Doc, error) {
	sq := map[string]any{"from": []any{map[string]any{"collectionId": collection}}}
	return c.runQuery(ctx, p, sq)
}

func (c *Client) runQuery(ctx context.Context, p Project, structuredQuery map[string]any) ([]Doc, error) {
	u := p.base() + ":runQuery?key=" + p.APIKey
	body := map[string]any{"structuredQuery": structuredQuery}

	// runQuery devuelve un array; usamos do2 para arrays.
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("firestore runQuery %d: %s", resp.StatusCode, string(data))
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

// UpdateField actualiza un único campo (updateDoc parcial). Equivale a
// updateUserData(docID, data, campo, db) del JS.
func (c *Client) UpdateField(ctx context.Context, p Project, path, field string, value any) error {
	return c.UpdateFields(ctx, p, path, map[string]any{field: value})
}

// UpdateFields actualiza varios campos a la vez (updateMask por cada campo).
func (c *Client) UpdateFields(ctx context.Context, p Project, path string, fields map[string]any) error {
	q := url.Values{}
	q.Set("key", p.APIKey)
	for k := range fields {
		q.Add("updateMask.fieldPaths", k)
	}
	u := p.base() + "/" + path + "?" + q.Encode()
	body := map[string]any{"fields": encodeFields(fields)}
	_, _, err := c.do(ctx, p, http.MethodPatch, u, body)
	return err
}

// SetDoc crea/reemplaza un documento con ID conocido (setDoc).
func (c *Client) SetDoc(ctx context.Context, p Project, path string, fields map[string]any) error {
	u := p.base() + "/" + path + "?key=" + p.APIKey
	body := map[string]any{"fields": encodeFields(fields)}
	_, _, err := c.do(ctx, p, http.MethodPatch, u, body)
	return err
}

// DeleteDoc elimina un documento por path (deleteDoc).
func (c *Client) DeleteDoc(ctx context.Context, p Project, path string) error {
	u := p.base() + "/" + path + "?key=" + p.APIKey
	_, _, err := c.do(ctx, p, http.MethodDelete, u, nil)
	return err
}

// AddDoc crea un documento con ID autogenerado (addDoc). Devuelve el ID nuevo.
func (c *Client) AddDoc(ctx context.Context, p Project, collection string, fields map[string]any) (string, error) {
	u := p.base() + "/" + collection + "?key=" + p.APIKey
	body := map[string]any{"fields": encodeFields(fields)}
	out, _, err := c.do(ctx, p, http.MethodPost, u, body)
	if err != nil {
		return "", err
	}
	if name, ok := out["name"].(string); ok {
		return idFromName(name), nil
	}
	return "", nil
}

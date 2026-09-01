package firebase

import (
	"context"
	"sync"
	"time"
)

// AppData es la respuesta de /datosApp (misma forma que el JS, incl. maisAPPs).
type AppData struct {
	DatosApp   map[string]any   `json:"datosApp"`
	Planes     []map[string]any `json:"planes"`
	PlanGrupal []map[string]any `json:"planGrupal"`
	Anuncios   []map[string]any `json:"anuncios"`
	Banners    []map[string]any `json:"banners"`
	MaisAPPs   any              `json:"maisAPPs"`
}

// GetAppData replica getAppData(db, {anuncios, banners, getAppsData}): lee datos/app +
// planes + planGrupal; para anuncios/banners usa los extras o Firestore; maisAPPs es la
// lista de "más apps" (getAppsData) pasada siempre.
func (c *Client) GetAppData(ctx context.Context, p Project, extraAnuncios, extraBanners []map[string]any, maisAPPs any) (AppData, error) {
	out := AppData{MaisAPPs: maisAPPs}

	// Estas lecturas no dependen entre sí. Hacerlas en serie convertía un cache
	// miss de Redis en cinco viajes consecutivos a Firestore.
	var (
		doc                Doc
		found              bool
		docErr             error
		planes, planGrupal []Doc
		anuncios, banners  []Doc
		wg                 sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		doc, found, docErr = c.GetDoc(ctx, p, "datos/app")
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		planes = c.safeCollection(ctx, p, "planes")
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		planGrupal = c.safeCollection(ctx, p, "planGrupal")
	}()
	if len(extraAnuncios) == 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			anuncios = c.safeCollection(ctx, p, "anuncios")
		}()
	}
	if len(extraBanners) == 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			banners = c.safeCollection(ctx, p, "banners")
		}()
	}
	wg.Wait()
	if docErr != nil {
		return out, docErr
	}
	if !found {
		out.DatosApp = map[string]any{
			"nombre":        "App",
			"version":       "0.0.0",
			"mantenimiento": false,
			"actualizado":   time.Now().UnixMilli(),
		}
	} else {
		out.DatosApp = doc.Data
	}

	out.Planes = docsToMaps(planes)
	out.PlanGrupal = docsToMaps(planGrupal)
	if len(extraAnuncios) > 0 {
		out.Anuncios = extraAnuncios
	} else {
		out.Anuncios = docsToMaps(anuncios)
	}
	if len(extraBanners) > 0 {
		out.Banners = extraBanners
	} else {
		out.Banners = docsToMaps(banners)
	}
	// Solo se muestran los activos: se descartan los items con activo == false.
	// Los que no traen el campo `activo` (p.ej. los banners) se conservan.
	out.Anuncios = filterActive(out.Anuncios)
	out.Banners = filterActive(out.Banners)
	return out, nil
}

// filterActive quita los elementos cuyo campo `activo` es explícitamente false.
// Si no tienen el campo, se mantienen (comportamiento seguro para banners).
func filterActive(items []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if v, ok := it["activo"]; ok {
			if b, isBool := v.(bool); isBool && !b {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// safeCollection lee una colección y, ante error, devuelve [] (como safeGetCollection del JS).
func (c *Client) safeCollection(ctx context.Context, p Project, name string) []Doc {
	docs, err := c.ListCollection(ctx, p, name)
	if err != nil {
		return nil
	}
	return docs
}

// docsToMaps añade el id a cada doc (como hace map(d => ({id, ...data}))).
func docsToMaps(docs []Doc) []map[string]any {
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		m := map[string]any{"id": d.ID}
		for k, v := range d.Data {
			m[k] = v
		}
		out = append(out, m)
	}
	return out
}

// BuscarNumeroTelefonico replica buscarNumeroTelefonico(numero): getDoc telefonos/{numero}.
func (c *Client) BuscarNumeroTelefonico(ctx context.Context, numero string) (string, error) {
	doc, found, err := c.GetDoc(ctx, c.Registry.Telefonos(), "telefonos/"+numero)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	if n, ok := doc.Data["nombre_completo"].(string); ok {
		return n, nil
	}
	return "", nil
}

// UpdateNumbers replica updateNumbers(numerosData): setDoc telefonos/{numero}
// con { nombre_completo } para cada item del arreglo.
func (c *Client) UpdateNumbers(ctx context.Context, numeros []map[string]any) error {
	p := c.Registry.Telefonos()
	for _, n := range numeros {
		numero, _ := n["numero"].(string)
		nombre := n["nombreCompleto"]
		if numero == "" {
			continue
		}
		if err := c.SetDoc(ctx, p, "telefonos/"+numero, map[string]any{"nombre_completo": nombre}); err != nil {
			return err
		}
	}
	return nil
}

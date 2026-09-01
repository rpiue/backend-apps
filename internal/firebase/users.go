package firebase

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// limaLoc es la zona horaria usada por toda la lógica de fechas (America/Lima).
func limaLoc() *time.Location {
	loc, err := time.LoadLocation("America/Lima")
	if err != nil {
		return time.UTC
	}
	return loc
}

// asTime extrae un time.Time de un campo decodificado (tsJSON) si es timestamp.
func asTime(v any) (time.Time, bool) {
	if ts, ok := v.(tsJSON); ok {
		return time.Unix(ts.Seconds, ts.Nanoseconds), true
	}
	return time.Time{}, false
}

// AsTime es la versión exportada de asTime, para que otros paquetes (handlers)
// puedan interpretar un campo timestamp decodificado de Firestore.
func AsTime(v any) (time.Time, bool) { return asTime(v) }

// ObtenerFechas replica obtenerFechas(): fecha actual y fecha final (+1 mes clamp).
func ObtenerFechas() (Timestamp, Timestamp) {
	now := time.Now().In(limaLoc())
	return Timestamp{Time: now}, Timestamp{Time: addOneMonthClamped(now)}
}

// addOneMonthClamped suma un mes; si el día se desborda, ajusta al último día del mes.
func addOneMonthClamped(t time.Time) time.Time {
	y, m, d := t.Date()
	lastDayNext := time.Date(y, m+2, 0, 0, 0, 0, 0, t.Location()).Day()
	if d > lastDayNext {
		d = lastDayNext
	}
	return time.Date(y, m+1, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// calcularFechaVencimiento replica la lógica del JS (extiende o reinicia el vencimiento).
func calcularFechaVencimiento(fechaFinal any, sinPlan bool) time.Time {
	loc := limaLoc()
	hoy := time.Now().In(loc)
	base := hoy
	if t, ok := asTime(fechaFinal); ok {
		base = t.In(loc)
	}
	var desde time.Time
	switch {
	case base.Before(hoy):
		desde = hoy
	case sinPlan:
		desde = hoy
	default:
		desde = base
	}
	// new Date(año, mes+1, nuevoDia) -> medianoche local
	y, m, d := desde.Date()
	lastDay := time.Date(y, m+2, 0, 0, 0, 0, 0, loc).Day()
	if d > lastDay {
		d = lastDay
	}
	return time.Date(y, m+1, d, 0, 0, 0, 0, loc)
}

// GetUserData replica getUserData(userID, db): getDoc usuarios/userID -> { data }.
func (c *Client) GetUserData(ctx context.Context, p Project, userID string) (map[string]any, error) {
	doc, found, err := c.GetDoc(ctx, p, "usuarios/"+userID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("Documento no encontrado")
	}
	return doc.Data, nil
}

// GetUserByEmail replica getUserByEmail(email, db, clave): consulta por email y,
// si se pasa clave, exige que coincida. Devuelve los usuarios como [{id, ...data}].
func (c *Client) GetUserByEmail(ctx context.Context, p Project, email string, clave *string) ([]map[string]any, error) {
	docs, err := c.QueryEqual(ctx, p, "usuarios", "email", email, 0)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("No se encontró ningún usuario con ese email")
	}
	results := []map[string]any{}
	for _, d := range docs {
		if clave == nil || fmt.Sprintf("%v", d.Data["clave"]) == *clave {
			m := map[string]any{"id": d.ID}
			for k, v := range d.Data {
				m[k] = v
			}
			results = append(results, m)
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("Email encontrado pero la clave no coincide")
	}
	return results, nil
}

// UpdateUserData replica updateUserData(docID, data, campo, db), incluyendo el
// manejo especial de arrays de dispositivos ("cambiarDevice" y "dispositivo[.x]")
// con transacción, igual que el server actual.
func (c *Client) UpdateUserData(ctx context.Context, p Project, docID, campo string, data any) error {
	path := "usuarios/" + docID
	_, found, err := c.GetDoc(ctx, p, path)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("El documento con ID: %s no existe.", docID)
	}

	isChangeDevice := campo == "cambiarDevice" || strings.HasPrefix(campo, "cambiarDevice.")
	isDevice := campo == "dispositivo" || strings.HasPrefix(campo, "dispositivo.") || strings.HasPrefix(campo, "dispositivos.")

	if isChangeDevice {
		return c.cambiarDeviceActivo(ctx, p, path, docID, data)
	}
	if isDevice {
		return c.registrarDispositivo(ctx, p, path, docID, campo, data)
	}
	return c.UpdateField(ctx, p, path, campo, data)
}

// deviceArray normaliza el campo "dispositivos" del doc a []map[string]any.
func deviceArray(current map[string]any) []map[string]any {
	out := []map[string]any{}
	raw, ok := current["dispositivos"].([]any)
	if !ok {
		return out
	}
	for _, d := range raw {
		if m, ok := d.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func toAnySlice(devices []map[string]any) []any {
	out := make([]any, len(devices))
	for i, d := range devices {
		out[i] = d
	}
	return out
}

// cambiarDeviceActivo replica el bloque isChangeDeviceCampo: marca inicio:true al
// dispositivo objetivo (y false a los demás), agregándolo si no existe.
func (c *Client) cambiarDeviceActivo(ctx context.Context, p Project, path, docID string, data any) error {
	targetID := ""
	switch v := data.(type) {
	case string:
		targetID = v
	case map[string]any:
		if id, ok := v["id"]; ok {
			targetID = fmt.Sprintf("%v", id)
		}
	}
	if targetID == "" {
		return fmt.Errorf("Para 'cambiarDevice', data debe ser un string id o {id: '...'}")
	}

	txn, err := c.BeginTransaction(ctx, p)
	if err != nil {
		return err
	}
	doc, found, err := c.GetDocTx(ctx, p, path, txn)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("El documento con ID: %s no existe.", docID)
	}
	devices := deviceArray(doc.Data)
	found = false
	for i, d := range devices {
		isTarget := fmt.Sprintf("%v", d["id"]) == targetID
		d["inicio"] = isTarget
		devices[i] = d
		if isTarget {
			found = true
		}
	}
	if !found {
		devices = append(devices, map[string]any{"id": targetID, "inicio": true})
	}
	return c.Commit(ctx, p, []Write{{
		Path:       path,
		Fields:     map[string]any{"dispositivos": toAnySlice(devices)},
		UpdateMask: []string{"dispositivos"},
	}}, txn)
}

// registrarDispositivo replica el bloque isDeviceCampo: registra/actualiza un
// dispositivo sin tocar el "inicio" de los demás, y fija dispositivosA si aplica.
func (c *Client) registrarDispositivo(ctx context.Context, p Project, path, docID, campo string, data any) error {
	dm, ok := data.(map[string]any)
	if !ok {
		return fmt.Errorf("Para 'dispositivo.*', data debe ser un objeto (map) del dispositivo.")
	}
	idVal, ok := dm["id"]
	if !ok || fmt.Sprintf("%v", idVal) == "" {
		return fmt.Errorf("Para 'dispositivo.*', data debe incluir un 'id'.")
	}
	deviceID := fmt.Sprintf("%v", idVal)

	var subKey string
	if parts := strings.SplitN(campo, ".", 2); len(parts) > 1 {
		subKey = parts[1]
	}

	txn, err := c.BeginTransaction(ctx, p)
	if err != nil {
		return err
	}
	doc, found, err := c.GetDocTx(ctx, p, path, txn)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("El documento con ID: %s no existe.", docID)
	}
	current := doc.Data
	devices := deviceArray(current)

	idx := -1
	for i, d := range devices {
		if fmt.Sprintf("%v", d["id"]) == deviceID {
			idx = i
			break
		}
	}

	base := map[string]any{}
	if idx >= 0 {
		base = devices[idx]
	}
	next := map[string]any{}
	for k, v := range base {
		next[k] = v
	}
	for k, v := range dm {
		next[k] = v
	}
	if subKey != "" {
		if val, ok := dm[subKey]; ok {
			next[subKey] = val
		} else if val, ok := dm["value"]; ok {
			next[subKey] = val
		}
	}
	next["id"] = deviceID

	if idx < 0 {
		next["inicio"] = len(devices) == 0
		devices = append(devices, next)
	} else {
		if prev, ok := base["inicio"]; ok {
			next["inicio"] = prev
		} else {
			next["inicio"] = len(devices) == 1 && idx == 0
		}
		devices[idx] = next
	}

	activeRaw, _ := current["dispositivosA"]
	hasActive := activeRaw != nil && strings.TrimSpace(fmt.Sprintf("%v", activeRaw)) != ""
	fields := map[string]any{"dispositivos": toAnySlice(devices)}
	mask := []string{"dispositivos"}
	if !hasActive && len(devices) == 1 {
		if b, ok := devices[0]["inicio"].(bool); ok && b {
			fields["dispositivosA"] = deviceID
			mask = append(mask, "dispositivosA")
		}
	}
	return c.Commit(ctx, p, []Write{{Path: path, Fields: fields, UpdateMask: mask}}, txn)
}

// EliminarUsuario replica eliminarUsuario(docID, db): soft-delete con sufijo -eliminado.
func (c *Client) EliminarUsuario(ctx context.Context, p Project, docID string) error {
	doc, found, err := c.GetDoc(ctx, p, "usuarios/"+docID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("El documento con ID: %s no existe.", docID)
	}
	emailActual, _ := doc.Data["email"].(string)
	emailNuevo := emailActual
	if !strings.HasSuffix(emailActual, "-eliminado") {
		emailNuevo = emailActual + "-eliminado"
	}
	return c.UpdateFields(ctx, p, "usuarios/"+docID, map[string]any{
		"eliminado": true,
		"inicio":    false,
		"refe":      false,
		"email":     emailNuevo,
		"updatedAt": Timestamp{Time: time.Now()},
	})
}

// GuardarUsuario replica guardarUsuario(usuario, db) con validación de duplicados
// y los campos por defecto. Devuelve el ID del documento creado.
func (c *Client) GuardarUsuario(ctx context.Context, p Project, u map[string]any) (string, error) {
	email, _ := u["email"].(string)
	telefono := u["telefono"]

	dupEmail, err := c.QueryEqual(ctx, p, "usuarios", "email", email, 1)
	if err != nil {
		return "", err
	}
	if len(dupEmail) > 0 {
		return "", fmt.Errorf("Ya existe un usuario con este correo.")
	}
	dupTel, err := c.QueryEqual(ctx, p, "usuarios", "telefono", telefono, 1)
	if err != nil {
		return "", err
	}
	if len(dupTel) > 0 {
		return "", fmt.Errorf("Ya existe un usuario con este teléfono.")
	}

	fechaActual, fechaFinal := ObtenerFechas()
	u["plan"] = "Sin Plan"
	// clave: conservar si vino (pin), si no, "".
	if s, ok := u["clave"].(string); !ok || s == "" {
		u["clave"] = ""
	}
	u["eliminado"] = false
	// inicio: conservar si vino true (apps con inicio, ej. bcp), si no false.
	if b, ok := u["inicio"].(bool); !ok || !b {
		u["inicio"] = false
	}
	// dispositivos: si vino un array [{id,...}], conservarlo y derivar dispositivosA;
	// si no, default {1:""} (replica datos-usuario.js).
	if disp, ok := u["dispositivos"]; ok && disp != nil {
		if arr, isArr := disp.([]any); isArr && len(arr) > 0 {
			if first, isMap := arr[0].(map[string]any); isMap {
				if id, hasID := first["id"]; hasID {
					u["dispositivosA"] = id
				}
			}
		}
	} else {
		u["dispositivos"] = map[string]any{"1": ""}
	}
	u["fecha_inicial"] = fechaActual
	u["fecha_final"] = fechaFinal
	u["contactos"] = map[string]any{}
	u["crear_contacto"] = false
	u["QR"] = false
	u["acceso"] = false
	u["pedir"] = false
	u["gmail"] = false
	u["bcp"] = false
	u["ruta"] = "/storage/emulated/0/DCIM/"
	u["sms"] = false
	u["vendedor"] = false
	u["email"] = strings.ToLower(email)

	return c.AddDoc(ctx, p, "usuarios", u)
}

// DarPlanResult informa el resultado y a quién notificar.
type DarPlanResult struct {
	Success     bool
	Message     string
	NotifyEmail string // email a quien avisar "plan activado" (vacío si no aplica)
	Plan        string
	FechaFinal  time.Time // nuevo vencimiento (cero si no se otorgó)
	Compras     int64     // veces que este usuario ha comprado un plan (tras sumar esta)
}

// DarPlanOpts son los extras opcionales (vendedor/referido) de darPlan.
type DarPlanOpts struct {
	EmailVendedor string
	IDVendedor    string
	// NoCount evita sumar 1 a comprasPlan. Se usa cuando el acceso NO es una
	// compra nueva (p.ej. re-otorgar a beneficiarios al regenerar códigos grupales).
	NoCount bool
}

// comprasComoInt normaliza el contador comprasPlan sin importar cómo lo guardó
// Firestore (int64/float64/string).
func comprasComoInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		if x, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return x
		}
	}
	return 0
}

// DarPlan replica darPlan(email, nuevoPlan, db, {...}): actualiza el doc del usuario,
// recalcula vencimiento y marca QR/contacto. La notificación la dispara el handler.
func (c *Client) DarPlan(ctx context.Context, p Project, email, nuevoPlan string, opts DarPlanOpts) (DarPlanResult, error) {
	docs, err := c.QueryEqual(ctx, p, "usuarios", "email", email, 0)
	if err != nil {
		return DarPlanResult{}, err
	}
	if len(docs) == 0 {
		return DarPlanResult{Success: false, Message: "❌ No existe el correo o tal vez fue eliminado."}, nil
	}

	data := docs[0].Data
	sinPlan := fmt.Sprintf("%v", data["plan"]) == "Sin Plan"
	nuevaFecha := calcularFechaVencimiento(data["fecha_final"], sinPlan)

	// Contador de compras del usuario (Firestore no tiene increment atómico en
	// esta capa REST; leemos el valor y sumamos 1). Solo cuenta compras reales.
	compras := comprasComoInt(data["comprasPlan"])
	if !opts.NoCount {
		compras++
	}

	nuevosDatos := map[string]any{
		"plan":           nuevoPlan,
		"QR":             true,
		"crear_contacto": true,
		"planN":          0,
		"fecha_final":    Timestamp{Time: nuevaFecha},
		"comprasPlan":    compras,
	}
	if opts.IDVendedor != "" {
		nuevosDatos["IDVendedor"] = opts.IDVendedor
	}
	if opts.EmailVendedor != "" {
		nuevosDatos["emailRefe"] = opts.EmailVendedor
		nuevosDatos["refe"] = true
	} else {
		nuevosDatos["refe"] = false
	}
	switch nuevoPlan {
	case "Medium":
		nuevosDatos["gmail"] = true
	case "Medium Grupal Plus":
		nuevosDatos["gmail"] = true
		nuevosDatos["planN"] = 3
	case "Medium Grupal":
		nuevosDatos["gmail"] = true
		nuevosDatos["planN"] = 2
	case "Basico Grupal":
		nuevosDatos["planN"] = 1
	}

	for _, d := range docs {
		if err := c.UpdateFields(ctx, p, "usuarios/"+d.ID, nuevosDatos); err != nil {
			return DarPlanResult{}, err
		}
	}
	return DarPlanResult{
		Success:     true,
		Message:     "✅ Plan y QR actualizados correctamente.",
		NotifyEmail: email,
		Plan:        nuevoPlan,
		FechaFinal:  nuevaFecha,
		Compras:     compras,
	}, nil
}

// ExisteUsuario replica existeUsuario({db, telefono, email}): devuelve `disponible`
// = true si NI el email NI el teléfono existen ya (es decir, está libre).
func (c *Client) ExisteUsuario(ctx context.Context, p Project, telefono, email string) (bool, error) {
	tel := strings.TrimSpace(telefono)
	em := strings.ToLower(strings.TrimSpace(email))
	if tel == "" && em == "" {
		return false, nil
	}
	if tel != "" {
		docs, err := c.QueryEqual(ctx, p, "usuarios", "telefono", tel, 1)
		if err == nil && len(docs) > 0 {
			return false, nil // existe -> no disponible
		}
	}
	if em != "" {
		docs, err := c.QueryEqual(ctx, p, "usuarios", "email", em, 1)
		if err == nil && len(docs) > 0 {
			return false, nil
		}
	}
	return true, nil // disponible
}

// UsuarioCRM busca un usuario por email y devuelve una ficha CRM:
// dispositivos conectados, fecha de creación, vencimiento y estado de la suscripción.
func (c *Client) UsuarioCRM(ctx context.Context, p Project, email string) (map[string]any, error) {
	docs, err := c.QueryEqual(ctx, p, "usuarios", "email", email, 1)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("usuario no encontrado")
	}
	d := docs[0]
	data := d.Data

	// Dispositivos conectados: cuenta valores no vacíos del mapa "dispositivos".
	dispositivos := 0
	var listaDisp []string
	if m, ok := data["dispositivos"].(map[string]any); ok {
		for _, v := range m {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				dispositivos++
				listaDisp = append(listaDisp, s)
			}
		}
	}
	if s, ok := data["dispositivosA"].(string); ok && strings.TrimSpace(s) != "" {
		listaDisp = append(listaDisp, s)
	}

	creada := millisOf(data["fecha_inicial"])
	vence := millisOf(data["fecha_final"])
	vencida := vence > 0 && time.UnixMilli(vence).Before(time.Now())

	eliminado, _ := data["eliminado"].(bool)

	return map[string]any{
		"id":                d.ID,
		"email":             data["email"],
		"nombre":            data["nombre"],
		"apellidos":         data["apellidos"],
		"telefono":          data["telefono"],
		"plan":              data["plan"],
		"dispositivos":      dispositivos,
		"listaDispositivos": listaDisp,
		"fechaCreacion":     creada,
		"fechaVencimiento":  vence,
		"vencida":           vencida,
		"eliminado":         eliminado,
	}, nil
}

// millisOf extrae milisegundos de un campo timestamp decodificado (tsJSON).
func millisOf(v any) int64 {
	if t, ok := asTime(v); ok {
		return t.UnixMilli()
	}
	return 0
}

// CambiarMovil replica cambiarMovil(email, idMovil, db): cambia dispositivo con
// límite de 30 días desde el último cambio.
func (c *Client) CambiarMovil(ctx context.Context, p Project, email, idMovil string) (DarPlanResult, error) {
	return c.cambiarMovil(ctx, p, email, idMovil, false)
}

// CambiarMovilBypass cambia el dispositivo SALTANDO el cooldown de 30 días
// (replica cambiarMovil(..., { bypassCooldown: true }) usado por el chat admin).
func (c *Client) CambiarMovilBypass(ctx context.Context, p Project, email, idMovil string) (DarPlanResult, error) {
	return c.cambiarMovil(ctx, p, email, idMovil, true)
}

func (c *Client) cambiarMovil(ctx context.Context, p Project, email, idMovil string, bypassCooldown bool) (DarPlanResult, error) {
	docs, err := c.QueryEqual(ctx, p, "usuarios", "email", email, 1)
	if err != nil {
		return DarPlanResult{}, err
	}
	if len(docs) == 0 {
		return DarPlanResult{Success: false, Message: "❌ No existe el correo o tal vez fue eliminado."}, nil
	}
	d := docs[0]
	if last, ok := asTime(d.Data["fecha_cambio_deveci"]); ok && !bypassCooldown {
		dias := time.Since(last).Hours() / 24
		if dias < 30 {
			diasLeft := int(30-dias) + 1
			return DarPlanResult{
				Success: false,
				Message: fmt.Sprintf("❌ Solo puedes cambiar el dispositivo cada 30 días. Por favor espera %d día(s) más.", diasLeft),
			}, nil
		}
	}
	err = c.UpdateFields(ctx, p, "usuarios/"+d.ID, map[string]any{
		"dispositivosA":       idMovil,
		"fecha_cambio_deveci": Timestamp{Time: time.Now()},
	})
	if err != nil {
		return DarPlanResult{}, err
	}
	return DarPlanResult{Success: true, Message: "✅ Dispositivo y fecha de cambio actualizados correctamente."}, nil
}

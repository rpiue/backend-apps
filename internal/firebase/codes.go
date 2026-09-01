package firebase

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Port de codigo.js. Trabaja sobre el proyecto controlPagos:
//   codigosApp/{email}                 (doc del vendedor)
//   codigosApp/{email}/codigos/{code}  (cada código)
//   codigosIndex/{code}                (índice global)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)
var nonLetters = regexp.MustCompile(`[^a-z]`)

func normEmailLower(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// planYcantidad replica planYcantidadPorOpcion: 1->Basico(1), 2->Medium(2), 3->Premium(4).
func planYcantidad(opcion int) (string, int, error) {
	switch opcion {
	case 1:
		return "Basico", 1, nil
	case 2:
		return "Medium", 2, nil
	case 3:
		return "Premium", 4, nil
	default:
		return "", 0, fmt.Errorf("Opción inválida (usa 1, 2 o 3)")
	}
}

// OpcionDePlanGrupal traduce el nombre de un plan grupal al `opcion`/planN que
// espera GenerarCodigosParaUsuario, con el mismo mapeo que resolvePlanDetails de
// index.js: Basico Grupal->1, Medium Grupal->2, Medium Grupal Plus->3. Devuelve
// false para los planes personales, que se otorgan con DarPlan a secas.
func OpcionDePlanGrupal(plan string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "basico grupal":
		return 1, true
	case "medium grupal":
		return 2, true
	case "medium grupal plus":
		return 3, true
	}
	return 0, false
}

func randInt(min, max int) int {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max-min)))
	return min + int(n.Int64())
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// genUniqueCode replica genUniqueCode(email).
func genUniqueCode(email string) string {
	base := nonAlnum.ReplaceAllString(normEmailLower(email), "")
	prefix := base
	if len(prefix) > 3 {
		prefix = prefix[:3]
	}
	for len(prefix) < 3 {
		prefix += "x"
	}
	six := fmt.Sprintf("%d", randInt(100000, 1000000))
	now := time.Now().UTC()
	stamp := fmt.Sprintf("%04d%02d%02d%02d%02d%02d%03d",
		now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond()/1e6)
	rand2 := randHex(1)[:2]
	return fmt.Sprintf("%s%s-%s%s", prefix, six, stamp, rand2)
}

func firstName(full string) string {
	f := strings.Fields(strings.TrimSpace(full))
	if len(f) == 0 {
		return "user"
	}
	return f[0]
}

// makeSellerId replica makeSellerId(fullName) en hora Lima.
func makeSellerId(fullName string) string {
	loc := limaLoc()
	now := time.Now().In(loc)
	clean := nonLetters.ReplaceAllString(strings.ToLower(firstName(fullName)), "")
	pref := clean
	if len(pref) > 4 {
		pref = pref[:4]
	}
	if len(pref) < 3 {
		pref = (pref + "xxx")[:3]
	}
	return fmt.Sprintf("%s%02d%02d%02d%02d%02d%02d", pref, now.Day(), int(now.Month()), now.Year()%100, now.Hour(), now.Minute(), now.Second())
}

func normalizeStatus(v any) string {
	switch s := v.(type) {
	case bool:
		if s {
			return "disponible"
		}
		return "usado"
	case string:
		return strings.ToLower(s)
	}
	return "desconocido"
}

// codigoVencimiento replica calcularFechaVencimiento() de codigo.js (now + 1 mes).
func codigoVencimiento() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month()+1, now.Day(), now.Hour(), now.Minute(), now.Second(), 0, now.Location())
}

// ListarCodigos replica listarCodigos(email): lista códigos del vendedor con email enmascarado.
func (c *Client) ListarCodigos(ctx context.Context, email string) ([]map[string]any, error) {
	p := c.Registry.ControlPagos()
	_email := normEmailLower(email)
	_, exists, err := c.GetDoc(ctx, p, "codigosApp/"+_email)
	if err != nil || !exists {
		return []map[string]any{}, err
	}
	docs, err := c.Query(ctx, p, "codigosApp/"+_email, "codigos", nil, 200)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, d := range docs {
		m := map[string]any{"id": d.ID}
		for k, v := range d.Data {
			m[k] = v
		}
		if correo, ok := d.Data["usedByEmail"].(string); ok && correo != "" {
			m["usedByEmail"] = maskEmail(correo)
		}
		out = append(out, m)
	}
	return out, nil
}

func maskEmail(correo string) string {
	parts := strings.SplitN(correo, "@", 2)
	if len(parts) != 2 {
		return correo
	}
	u, dom := parts[0], parts[1]
	if len(u) >= 4 {
		return u[:3] + "****" + u[len(u)-1:] + "@" + dom
	}
	return u[:1] + "****@" + dom
}

// VerificarCodigoOnline replica verificarCodigoOnline(code): valida sin consumir.
func (c *Client) VerificarCodigoOnline(ctx context.Context, code string) map[string]any {
	p := c.Registry.ControlPagos()
	_code := strings.TrimSpace(code)
	if _code == "" {
		return map[string]any{"ok": false, "message": "Código requerido"}
	}
	idx, ok, err := c.GetDoc(ctx, p, "codigosIndex/"+_code)
	if err != nil || !ok {
		return map[string]any{"ok": false, "message": "Código inválido o no indexado"}
	}
	path, _ := idx.Data["path"].(string)
	if path == "" {
		return map[string]any{"ok": false, "message": "Índice corrupto: falta path"}
	}
	ownerEmail, _ := idx.Data["ownerEmail"].(string)
	if ownerEmail == "" {
		ownerEmail = ownerFromPath(path)
	}
	codeDoc, ok, err := c.GetDoc(ctx, p, path)
	if err != nil || !ok {
		return map[string]any{"ok": false, "message": "Código no existe (índice desincronizado)"}
	}
	status := normalizeStatus(codeDoc.Data["status"])
	if status != "disponible" {
		return map[string]any{"ok": false, "message": "Código no disponible", "data": codeDoc.Data, "ownerEmail": ownerEmail}
	}
	if exp, ok := asTime(codeDoc.Data["expiresAt"]); ok && exp.Before(time.Now()) {
		return map[string]any{"ok": false, "message": "Código expirado", "data": codeDoc.Data, "ownerEmail": ownerEmail}
	}
	owner := map[string]any{"email": ownerEmail}
	if od, ok, _ := c.GetDoc(ctx, p, "codigosApp/"+ownerEmail); ok {
		for k, v := range od.Data {
			owner[k] = v
		}
	}
	data := map[string]any{}
	for k, v := range codeDoc.Data {
		data[k] = v
	}
	data["code"] = _code
	data["status"] = status
	return map[string]any{"ok": true, "message": "Código disponible", "data": data, "owner": owner}
}

func ownerFromPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// ConsumirResult informa el resultado y a quién notificar (lo hace el handler).
type ConsumirResult struct {
	Body         map[string]any // respuesta a devolver al cliente
	NotifyOwner  string         // email del vendedor a notificar (vacío si no aplica)
	NotifyClient string         // email del cliente a notificar "plan activado"
	Plan         string
}

// ConsumirCodigo replica consumirCodigo(code, db, {deviceId, emailCliente}) de
// codigo.js. Las respuestas son las MISMAS que las del JS, campo a campo, para
// que la app móvil no note ninguna diferencia al cambiar de backend.
//
// Correspondencia con el JS (codigo.js:403):
//
//	índice sin documento     -> "Código inválido"        + submessage
//	índice sin path          -> "Índice corrupto: falta path"
//	código sin documento     -> "Código no existe"
//	status no disponible     -> "Código no disponible"   + submessage + owner
//	expiresAt pasado         -> "Código expirado"        + submessage + owner
//	vendedor dado de baja    -> "<nombre> Ya no es Owner"
//	darPlan sin éxito        -> resultado VACÍO (el handler responde 404, igual que el JS)
//	error en la transacción  -> "Error al consumir el código"
//	éxito                    -> ok:true + plan + message + submessage
func (c *Client) ConsumirCodigo(ctx context.Context, userDB Project, code, deviceID, emailCliente string) (ConsumirResult, error) {
	p := c.Registry.ControlPagos()
	_code := strings.TrimSpace(code)
	log.Printf("[consumirCode] iniciando: code=%q device=%q email=%q userDB=%q project=%q", _code, deviceID, emailCliente, userDB.ID, p.ID)
	if _code == "" {
		log.Printf("[consumirCode] código vacío")
		return ConsumirResult{Body: map[string]any{"ok": false, "message": "Código requerido"}}, nil
	}

	log.Printf("[consumirCode] consultando índice Firestore: project=%s path=%s", p.ID, "codigosIndex/"+_code)
	// El getDoc del índice queda FUERA del try del JS, así que un error aquí
	// sube al handler y acaba en un 500. Se mantiene igual.
	idx, ok, err := c.GetDoc(ctx, p, "codigosIndex/"+_code)
	if err != nil {
		log.Printf("[consumirCode] error leyendo índice: code=%q err=%v", _code, err)
		return ConsumirResult{}, err
	}
	if !ok {
		log.Printf("[consumirCode] código NO existe en índice: %q", _code)
		return ConsumirResult{Body: map[string]any{
			"ok": false, "message": "Código inválido",
			"submessage": "El codigo no existe intentalo con otro codigo o compra un plan para poder tener acceso al app.",
		}}, nil
	}
	path, _ := idx.Data["path"].(string)
	if path == "" {
		log.Printf("[consumirCode] índice corrupto: code=%q idx=%v", _code, idx.Data)
		return ConsumirResult{Body: map[string]any{"ok": false, "message": "Índice corrupto: falta path"}}, nil
	}
	ownerEmail, _ := idx.Data["ownerEmail"].(string)
	if ownerEmail == "" {
		ownerEmail = ownerFromPath(path)
	}
	log.Printf("[consumirCode] índice ok: code=%q path=%q ownerEmail=%q", _code, path, ownerEmail)

	// A partir de aquí es el equivalente del runTransaction del JS: los fallos
	// internos no suben como error, se convierten en "Error al consumir el
	// código" con HTTP 200, igual que hace el catch del JS.
	log.Printf("[consumirCode] iniciando transacción Firestore para %q", _code)
	txn, err := c.BeginTransaction(ctx, p)
	if err != nil {
		log.Printf("[consumirCode] beginTransaction falló: code=%q err=%v", _code, err)
		return ConsumirResult{Body: errorAlConsumir()}, nil
	}
	log.Printf("[consumirCode] transacción abierta OK: code=%q txn=%q", _code, txn)
	// runTransaction del SDK de JS cierra la transacción sola cuando el callback
	// sale sin escribir. Aquí hay que hacerlo a mano: sin esto, cada código
	// rechazado deja una transacción abierta reteniendo bloqueos hasta que
	// Firestore la expira (~60 s). commited evita cancelar lo ya escrito.
	commited := false
	defer func() {
		if !commited {
			c.Rollback(ctx, p, txn)
		}
	}()

	log.Printf("[consumirCode] consultando doc del código: project=%s path=%s txn=%q", p.ID, path, txn)
	fresh, ok, err := c.GetDocTx(ctx, p, path, txn)
	if err != nil {
		log.Printf("[consumirCode] error leyendo documento del código: code=%q path=%q project=%s txn=%q err=%v", _code, path, p.ID, txn, err)
		return ConsumirResult{Body: errorAlConsumir()}, nil
	}
	if !ok {
		log.Printf("[consumirCode] documento del código no existe: code=%q path=%q project=%s txn=%q", _code, path, p.ID, txn)
		return ConsumirResult{Body: map[string]any{"ok": false, "message": "Código no existe"}}, nil
	}

	log.Printf("[consumirCode] consultando owner: project=%s path=%s txn=%q", p.ID, "codigosApp/"+ownerEmail, txn)
	ownerDoc, _, err := c.GetDocTx(ctx, p, "codigosApp/"+ownerEmail, txn)
	if err != nil {
		log.Printf("[consumirCode] error leyendo owner: code=%q ownerEmail=%q project=%s txn=%q err=%v", _code, ownerEmail, p.ID, txn, err)
		return ConsumirResult{Body: errorAlConsumir()}, nil
	}
	ownerName, _ := ownerDoc.Data["name"].(string)
	plan, _ := fresh.Data["plan"].(string)
	log.Printf("[consumirCode] doc encontrado: code=%q plan=%q status=%v expiresAt=%v ownerName=%q ownerStatus=%v", _code, plan, fresh.Data["status"], fresh.Data["expiresAt"], ownerName, ownerDoc.Data["status"])

	// El JS comprueba `typeof d.status === 'boolean' && d.status === true`: solo
	// el booleano true habilita el código. Un status en texto NO vale, ni
	// siquiera "disponible". Se replica tal cual para no aceptar códigos que el
	// backend actual rechazaría.
	disponible, _ := fresh.Data["status"].(bool)
	if !disponible {
		log.Printf("[consumirCode] código no disponible: code=%q status=%v plan=%q owner=%q", _code, fresh.Data["status"], plan, ownerName)
		return ConsumirResult{Body: map[string]any{
			"ok": false, "message": "Código no disponible",
			"submessage": fmt.Sprintf("El codigo para el plan %s ya se esta usando, intenta con otro codigo o compra un plan para poder tener acceso a los permisos.", plan),
			"owner":      ownerName,
		}}, nil
	}

	if exp, ok := asTime(fresh.Data["expiresAt"]); ok && exp.Before(time.Now()) {
		log.Printf("[consumirCode] código expirado: code=%q exp=%v now=%v plan=%q", _code, exp, time.Now(), plan)
		return ConsumirResult{Body: map[string]any{
			"ok": false, "message": "Código expirado",
			"submessage": fmt.Sprintf("El codigo para el plan %s ya expiro, intentalo con otro codigo o compra un plan para poder tener acceso al app.", plan),
			"owner":      ownerName,
		}}, nil
	}

	ownerActivo, _ := ownerDoc.Data["status"].(bool)
	if !ownerActivo {
		log.Printf("[consumirCode] owner dado de baja: code=%q owner=%q ownerStatus=%v", _code, ownerName, ownerDoc.Data["status"])
		return ConsumirResult{Body: map[string]any{"ok": false, "message": ownerName + " Ya no es Owner"}}, nil
	}

	// Dar acceso al cliente (darPlan sobre la DB de la app).
	acceso, err := c.DarPlan(ctx, userDB, emailCliente, plan, DarPlanOpts{EmailVendedor: ownerName})
	if err != nil || !acceso.Success {
		// El JS sale del callback SIN devolver nada cuando acceso.success no es
		// true, así que el resultado llega vacío al handler y este responde 404
		// con "Usuario no encontrado o no se pudo actualizar el dato.". Un Body
		// nil reproduce ese mismo camino. El código NO se marca como usado.
		if err != nil {
			log.Printf("[consumir] darPlan falló para %s (plan %s): %v", emailCliente, plan, err)
		}
		return ConsumirResult{}, nil
	}

	// Marcar el código como usado + espejar en el índice (atómico).
	now := Timestamp{Time: time.Now()}
	codeUpd := map[string]any{
		"status": false, "usedAt": now, "usedByEmail": emailCliente,
		"usedByDevice": deviceID, "updatedAt": now,
	}
	idxUpd := map[string]any{
		"status": false, "usedAt": now, "usedByEmail": emailCliente,
		"usedByDevice": deviceID, "updatedAt": now,
	}
	writes := []Write{
		{Path: path, Fields: codeUpd, UpdateMask: keysOf(codeUpd)},
		{Path: "codigosIndex/" + _code, Fields: idxUpd, UpdateMask: keysOf(idxUpd)},
	}
	if err := c.Commit(ctx, p, writes, txn); err != nil {
		return ConsumirResult{Body: errorAlConsumir()}, nil
	}
	commited = true

	return ConsumirResult{
		Body: map[string]any{
			"ok": true, "plan": plan, "message": "Código consumido",
			"submessage": fmt.Sprintf("Plan %s activado mediante el codigo de %s", plan, ownerName),
		},
		NotifyOwner:  ownerEmail,
		NotifyClient: acceso.NotifyEmail,
		Plan:         plan,
	}, nil
}

// errorAlConsumir es lo que el catch de runTransaction devuelve en el JS.
func errorAlConsumir() map[string]any {
	return map[string]any{"ok": false, "message": "Error al consumir el código"}
}

// keysOf devuelve las claves de un mapa de campos, para usarlas como
// updateMask de Firestore (actualización parcial: solo esos campos, el resto
// del documento se queda como está).
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

package firebase

import (
	"context"
	"fmt"
	"time"
)

// GenerarCodigosResult informa qué se otorgó al vendedor de un plan grupal:
// el plan base que recibe él mismo, su vencimiento y los códigos que puede
// repartir (los renovados + los recién creados).
type GenerarCodigosResult struct {
	Success    bool
	Message    string
	Plan       string    // plan base otorgado al vendedor (Basico/Medium/Premium)
	FechaFinal time.Time // vencimiento del plan del vendedor
	IDVendedor string
	Codigos    []string // todos los códigos vigentes del plan
	Creados    int      // cuántos se crearon en esta llamada
	// Beneficiarios son los clientes que ya habían consumido un código y a los
	// que se les renovó el acceso; el llamador debe avisarles igual que al vendedor.
	Beneficiarios []string
}

// GenerarCodigosParaUsuario replica generarCodigosParaUsuario(email, name, opcion, {db}):
// renueva los códigos disponibles del plan y crea los que falten hasta `cantidad`,
// manteniendo el índice global; luego re-otorga acceso a quienes ya consumieron.
// userDB es la DB del usuario (para darPlan). Notificar al vendedor es tarea del
// llamador, con los datos del resultado.
func (c *Client) GenerarCodigosParaUsuario(ctx context.Context, userDB Project, email, name string, opcion int) (GenerarCodigosResult, error) {
	p := c.Registry.ControlPagos()
	_email := normEmailLower(email)
	if _email == "" {
		return GenerarCodigosResult{}, errReq("Email requerido")
	}
	plan, cantidad, err := planYcantidad(opcion)
	if err != nil {
		return GenerarCodigosResult{}, err
	}

	userPath := "codigosApp/" + _email
	userSnap, exists, err := c.GetDoc(ctx, p, userPath)
	if err != nil {
		return GenerarCodigosResult{}, err
	}
	var idVendedor string
	if exists {
		idVendedor, _ = userSnap.Data["IDVendedor"].(string)
	} else {
		idVendedor = makeSellerId(name)
		if err := c.SetDoc(ctx, p, userPath, map[string]any{
			"status": true, "email": _email, "name": firstName(name),
			"IDVendedor": idVendedor, "createdAt": Timestamp{Time: time.Now()},
		}); err != nil {
			return GenerarCodigosResult{}, err
		}
	}

	// El vendedor obtiene el plan.
	acceso, err := c.DarPlan(ctx, userDB, email, plan, DarPlanOpts{IDVendedor: idVendedor})
	if err != nil {
		return GenerarCodigosResult{}, err
	}
	if !acceso.Success {
		return GenerarCodigosResult{Success: false, Message: acceso.Message}, nil
	}

	// Códigos disponibles existentes del plan.
	disponibles, err := c.Query(ctx, p, userPath, "codigos", []Filter{{Field: "plan", Op: "EQUAL", Value: plan}}, 0)
	if err != nil {
		return GenerarCodigosResult{}, err
	}

	expires := Timestamp{Time: codigoVencimiento()}
	now := Timestamp{Time: time.Now()}
	var writes []Write
	var codigos []string

	// Renovar expiración de los disponibles + espejo en índice.
	for _, d := range disponibles {
		codePath := userPath + "/codigos/" + d.ID
		if code, ok := d.Data["code"].(string); ok && code != "" {
			codigos = append(codigos, code)
		}
		writes = append(writes, Write{Path: codePath, Fields: map[string]any{
			"expiresAt": expires, "updatedAt": now,
		}, UpdateMask: []string{"expiresAt", "updatedAt"}})

		writes = append(writes, Write{Path: "codigosIndex/" + d.ID, Fields: map[string]any{
			"ownerEmail": _email, "plan": d.Data["plan"], "path": codePath,
			"status": d.Data["status"], "expiresAt": expires, "updatedAt": now,
			"createdAt": orNow(d.Data["createdAt"]),
		}})
	}

	// Crear los que falten hasta `cantidad`.
	faltan := cantidad - len(disponibles)
	for i := 0; i < faltan; i++ {
		code := genUniqueCode(_email)
		codePath := userPath + "/codigos/" + code
		codigos = append(codigos, code)
		writes = append(writes, Write{Path: codePath, Fields: map[string]any{
			"n": opcion, "code": code, "plan": plan, "status": true,
			"createdAt": now, "updatedAt": now, "expiresAt": expires,
			"usedAt": nil, "usedByEmail": nil, "usedByDevice": nil,
		}})
		writes = append(writes, Write{Path: "codigosIndex/" + code, Fields: map[string]any{
			"ownerEmail": _email, "plan": plan, "path": codePath, "status": true,
			"expiresAt": expires, "createdAt": now, "updatedAt": now,
		}})
	}

	if len(writes) > 0 {
		if err := c.Commit(ctx, p, writes, ""); err != nil {
			return GenerarCodigosResult{}, err
		}
	}

	res := GenerarCodigosResult{
		Success: true,
		Message: fmt.Sprintf("✅ Plan %s activado con %d código(s) de activación.", plan, len(codigos)),
		Plan:    plan, FechaFinal: acceso.FechaFinal, IDVendedor: idVendedor,
		Codigos: codigos, Creados: max(faltan, 0),
	}

	// Re-otorgar acceso a beneficiarios que ya consumieron códigos de este plan.
	todos, err := c.Query(ctx, p, userPath, "codigos", []Filter{{Field: "plan", Op: "EQUAL", Value: plan}}, 0)
	if err != nil {
		return res, err
	}
	beneficiarios := map[string]bool{}
	for _, d := range todos {
		consumido := normalizeStatus(d.Data["status"]) == "usado"
		if correo, ok := d.Data["usedByEmail"].(string); ok && consumido && correo != "" {
			beneficiarios[normEmailLower(correo)] = true
		}
	}
	for correo := range beneficiarios {
		if r, err := c.DarPlan(ctx, userDB, correo, plan, DarPlanOpts{EmailVendedor: name, NoCount: true}); err == nil && r.Success {
			res.Beneficiarios = append(res.Beneficiarios, correo)
		}
	}
	return res, nil
}

func orNow(v any) any {
	if v == nil {
		return Timestamp{Time: time.Now()}
	}
	return v
}

type reqErr string

func (e reqErr) Error() string { return string(e) }
func errReq(s string) error    { return reqErr(s) }

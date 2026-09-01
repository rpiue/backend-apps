// Package genbin replica genBIN.js: genera tarjetas (Luhn) y cuentas bancarias
// peruanas. Se usa al crear usuarios en apps que no son "yape" (bcp, interbank).
package genbin

import (
	"fmt"
	"math/rand"
	"strings"
)

// Tarjeta es una tarjeta generada.
type Tarjeta struct {
	Tipo           string `json:"tipo"`
	NumeroCompleto string `json:"numeroCompleto"`
	Mes            string `json:"mes"`
	Anio           int    `json:"anio"`
	UltimosCuatro  string `json:"ultimosCuatro"`
}

// Banco es una cuenta bancaria generada.
type Banco struct {
	Entidad      string `json:"entidad"`
	CuentaSimple string `json:"cuentaSimple"`
	CCI          string `json:"cci"`
	CCIFormato   string `json:"cciFormato"`
	Moneda       string `json:"moneda"`
}

func luhnOK(num string) bool {
	sum := 0
	n := len(num)
	for i := n - 1; i >= 0; i-- {
		d := int(num[i] - '0')
		if (n-1-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// GenerarTarjetas replica generarTarjetas(digi, cantidad). digi = BIN/inicio.
func GenerarTarjetas(digi string, cantidad int) []Tarjeta {
	if digi == "" {
		digi = "4"
	}
	if cantidad <= 0 {
		cantidad = 1
	}
	tipo, largo := "Desconocida", 16
	switch digi[0] {
	case '4':
		tipo = "Visa"
	case '5':
		tipo = "Mastercard"
	case '3':
		tipo, largo = "Amex", 15
	case '6':
		tipo = "Discover"
	}

	out := []Tarjeta{}
	for len(out) < cantidad {
		mes := fmt.Sprintf("%02d", rand.Intn(12)+1)
		anio := rand.Intn(2035-2024+1) + 2024
		num := digi
		for len(num) < largo {
			num += fmt.Sprintf("%d", rand.Intn(10))
		}
		if !luhnOK(num) {
			continue
		}
		dup := false
		for _, t := range out {
			if t.NumeroCompleto == num {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, Tarjeta{
			Tipo: tipo, NumeroCompleto: num, Mes: mes, Anio: anio,
			UltimosCuatro: num[len(num)-4:],
		})
	}
	return out
}

type bancoCfg struct {
	cod      string
	longitud int
	prefijo  string
}

var bancos = map[string]bancoCfg{
	"BCP":        {"002", 14, "300"},
	"INTERBANK":  {"003", 13, "89"},
	"BBVA":       {"011", 18, "0011"},
	"SCOTIABANK": {"009", 10, "000"},
	"BANBIF":     {"038", 10, "00"},
}

// GenerarBanco replica generarBanco(bancoPref, cantidad).
func GenerarBanco(bancoPref string, cantidad int) []Banco {
	if cantidad <= 0 {
		cantidad = 1
	}
	key := strings.ToUpper(strings.TrimSpace(bancoPref))
	cfg, ok := bancos[key]
	if !ok {
		key, cfg = "BCP", bancos["BCP"]
	}
	out := []Banco{}
	for i := 0; i < cantidad; i++ {
		cuenta := cfg.prefijo
		for len(cuenta) < cfg.longitud {
			cuenta += fmt.Sprintf("%d", rand.Intn(10))
		}
		plaza := fmt.Sprintf("%d", rand.Intn(800)+100)
		cuentaCCI := cuenta
		if len(cuentaCCI) > 12 {
			cuentaCCI = cuentaCCI[len(cuentaCCI)-12:]
		}
		for len(cuentaCCI) < 12 {
			cuentaCCI = "0" + cuentaCCI
		}
		dc := fmt.Sprintf("%d", rand.Intn(90)+10)
		moneda := "Dolares"
		if rand.Float64() > 0.5 {
			moneda = "Soles"
		}
		out = append(out, Banco{
			Entidad: key, CuentaSimple: cuenta,
			CCI:        cfg.cod + plaza + cuentaCCI + dc,
			CCIFormato: cfg.cod + "-" + plaza + "-" + cuentaCCI + "-" + dc,
			Moneda:     moneda,
		})
	}
	return out
}

package handlers

import (
	"testing"
	"time"
)

func TestBloqueoAuthPara_Escalones(t *testing.T) {
	casos := []struct {
		fallos int
		quiere time.Duration
	}{
		{0, 0}, {4, 0}, // margen para el usuario que se equivoca
		{5, time.Minute}, {7, 5 * time.Minute}, {10, 15 * time.Minute},
		{15, 30 * time.Minute}, {20, time.Hour}, {99999, time.Hour}, // con techo
	}
	for _, c := range casos {
		if got := bloqueoAuthPara(c.fallos); got != c.quiere {
			t.Errorf("bloqueoAuthPara(%d)=%s, quería %s", c.fallos, got, c.quiere)
		}
	}
}

func TestOfuscarCuenta_NoFiltraElEmailCompleto(t *testing.T) {
	casos := map[string]string{
		"juanperez@gmail.com": "ju***@gmail.com",
		"ab@x.com":            "***@x.com",
		"sinarroba":           "si***",
	}
	for entrada, quiere := range casos {
		if got := ofuscarCuenta(entrada); got != quiere {
			t.Errorf("ofuscarCuenta(%q)=%q, quería %q", entrada, got, quiere)
		}
	}
}

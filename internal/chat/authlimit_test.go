package chat

import (
	"testing"
	"time"
)

// La pregunta que de verdad importa: con estos tramos, ¿cuánto tarda alguien en
// recorrer el millón de combinaciones de un PIN de 6 dígitos?
func TestBloqueoProgresivo_PINDe6DigitosEsInviable(t *testing.T) {
	const combinaciones = 1_000_000

	// Se simula un atacante que va probando sin parar: cada fallo suma, y cuando
	// toca bloqueo tiene que esperar antes del siguiente intento.
	var espera time.Duration
	intentos := 0
	for n := 1; n <= 500; n++ {
		intentos++
		espera += bloqueoPara(n)
	}

	// En 500 intentos ya acumula muchísima espera.
	if espera < 400*time.Hour {
		t.Fatalf("500 intentos solo cuestan %s de espera; el bloqueo es demasiado laxo", espera)
	}

	// Extrapolación al espacio completo del PIN.
	porIntento := espera / time.Duration(intentos)
	total := porIntento * time.Duration(combinaciones)
	anios := total.Hours() / 24 / 365
	if anios < 50 {
		t.Fatalf("recorrer el PIN entero costaría ~%.0f años; se busca que sea inviable", anios)
	}
	t.Logf("coste medio por intento: %s → recorrer 10^6 PIN ≈ %.0f años", porIntento, anios)
}

func TestBloqueoPara_Escalones(t *testing.T) {
	casos := []struct {
		fallos int
		quiere time.Duration
	}{
		{0, 0}, {1, 0}, {4, 0}, // los primeros errores no molestan a nadie
		{5, 1 * time.Minute},
		{6, 1 * time.Minute},
		{7, 5 * time.Minute},
		{9, 5 * time.Minute},
		{10, 15 * time.Minute},
		{14, 15 * time.Minute},
		{15, 30 * time.Minute},
		{19, 30 * time.Minute},
		{20, 1 * time.Hour},
		{1000, 1 * time.Hour}, // techo: nunca se bloquea para siempre
	}
	for _, c := range casos {
		if got := bloqueoPara(c.fallos); got != c.quiere {
			t.Errorf("bloqueoPara(%d)=%s, quería %s", c.fallos, got, c.quiere)
		}
	}
}

// Un usuario legítimo que se equivoca un par de veces no debe notar nada: si a
// la tercera se le bloqueara la cuenta, la defensa sería peor que el ataque.
func TestBloqueoProgresivo_NoMolestaAlUsuarioNormal(t *testing.T) {
	for n := 1; n <= 4; n++ {
		if d := bloqueoPara(n); d != 0 {
			t.Fatalf("con %d fallos ya bloquea (%s); un usuario normal se equivoca varias veces", n, d)
		}
	}
}

// El techo de una hora es deliberado: sin él, cualquiera que conozca un email
// podría dejar a esa persona fuera de su cuenta para siempre fallando adrede.
func TestBloqueoProgresivo_TieneTecho(t *testing.T) {
	maximo := bloqueoPara(1_000_000)
	if maximo > time.Hour {
		t.Fatalf("el bloqueo máximo es %s; un bloqueo largo convierte la defensa en una forma de dejar fuera al usuario legítimo", maximo)
	}
	if maximo == 0 {
		t.Fatal("no hay bloqueo máximo definido")
	}
}

// El email se normaliza para que no den dos cupos por la misma cuenta.
func TestNormalizarCuenta(t *testing.T) {
	casos := map[string]string{
		"  Usuario@Ejemplo.COM ": "usuario@ejemplo.com",
		"USUARIO@EJEMPLO.COM":    "usuario@ejemplo.com",
		"usuario@ejemplo.com":    "usuario@ejemplo.com",
		"   ":                    "",
	}
	for entrada, quiere := range casos {
		if got := normalizarCuenta(entrada); got != quiere {
			t.Errorf("normalizarCuenta(%q)=%q, quería %q", entrada, got, quiere)
		}
	}
	// Las tres variantes deben compartir contador.
	a := normalizarCuenta("Ana@Test.com")
	b := normalizarCuenta("  ANA@TEST.COM  ")
	if a != b {
		t.Fatalf("%q y %q no comparten contador: se podrían duplicar los intentos", a, b)
	}
}

// Los logs no deben dejar la lista completa de correos de los usuarios.
func TestOfuscar(t *testing.T) {
	casos := map[string]string{
		"usuario@ejemplo.com": "us***@ejemplo.com",
		"ab@x.com":            "***@x.com",
		"a@x.com":             "***@x.com",
		"sinarroba":           "si***",
		"ab":                  "***",
	}
	for entrada, quiere := range casos {
		if got := ofuscar(entrada); got != quiere {
			t.Errorf("ofuscar(%q)=%q, quería %q", entrada, got, quiere)
		}
	}
	// Nunca debe salir el usuario completo.
	if got := ofuscar("juanperez@gmail.com"); got == "juanperez@gmail.com" {
		t.Fatal("el email salió sin ofuscar")
	}
}

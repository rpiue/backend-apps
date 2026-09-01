package secnet

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// El caso que justifica el paquete: una URL http:// hacia un host PÚBLICO se
// bloquea antes de enviar nada. Si esto se rompiera, un BTCPAY_URL mal escrito
// mandaría la API key en claro por internet sin que nadie lo notara.
func TestClient_BloqueaTextoPlanoPublico(t *testing.T) {
	c := Client(5 * time.Second)
	_, err := c.Get("http://example.com/algo")
	if !errors.Is(err, ErrPlaintext) {
		t.Fatalf("se esperaba ErrPlaintext, se obtuvo: %v", err)
	}
}

// Contrapunto necesario: los servicios internos (Ollama en la red de Docker)
// hablan http y DEBEN seguir funcionando, porque ese tráfico no sale del host.
func TestClient_PermiteTextoPlanoLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := Client(5 * time.Second).Get(srv.URL) // http://127.0.0.1:puerto
	if err != nil {
		t.Fatalf("el tráfico plano a loopback debe permitirse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Un servidor que redirige https -> http es la maniobra clásica para leer la
// petición siguiente. El cliente debe negarse a seguirla.
func TestClient_BloqueaDowngradeDeRedirect(t *testing.T) {
	plano := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plano.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plano.URL, http.StatusFound) // https -> http
	}))
	defer tlsSrv.Close()

	c := Client(5 * time.Second)
	// Se confía en el certificado autofirmado del servidor de prueba SOLO aquí,
	// para poder ejercitar la lógica de redirect sin montar una CA.
	clientTLS := tlsSrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	c.Transport = guard{next: &http.Transport{TLSClientConfig: clientTLS}}

	_, err := c.Get(tlsSrv.URL)
	if !errors.Is(err, ErrDowngrade) {
		t.Fatalf("se esperaba ErrDowngrade, se obtuvo: %v", err)
	}
}

// La verificación de certificados NUNCA debe estar desactivada: es lo único que
// distingue al servidor real de un atacante que presenta su propio certificado.
func TestTLS_VerificacionSiempreActiva(t *testing.T) {
	cfg := baseTLS()
	if cfg.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify debe ser false: con true, cualquiera puede suplantar al servidor")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion debe ser al menos TLS 1.2, es %x", cfg.MinVersion)
	}
}

func TestServerTLSConfig(t *testing.T) {
	cfg := ServerTLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("el servidor debe exigir TLS 1.2 como mínimo, exige %x", cfg.MinVersion)
	}
	// Todas las suites configuradas deben tener forward secrecy (ECDHE): sin
	// ella, quien guarde el tráfico hoy puede descifrarlo si la clave privada
	// se filtra mañana.
	for _, s := range cfg.CipherSuites {
		nombre := tls.CipherSuiteName(s)
		if len(nombre) < 5 || nombre[:11] != "TLS_ECDHE_R" && nombre[:11] != "TLS_ECDHE_E" {
			t.Errorf("suite sin forward secrecy: %s", nombre)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	casos := map[string]bool{
		"127.0.0.1":   true,
		"10.5.0.3":    true,
		"172.17.0.2":  true, // red por defecto de Docker
		"192.168.1.1": true,
		"8.8.8.8":     false,
		"1.1.1.1":     false,
	}
	for ip, want := range casos {
		if got := isPrivateIP(parseIP(t, ip)); got != want {
			t.Errorf("isPrivateIP(%s)=%v, quería %v", ip, got, want)
		}
	}
}

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("IP inválida en el test: %s", s)
	}
	return ip
}

// --- SSRF -------------------------------------------------------------------

// El endpoint de subida por URL hace que el SERVIDOR abra una conexión a donde
// diga otra persona. Estas direcciones son exactamente las que un atacante
// pondría para llegar a lo que él no alcanza.
func TestClientePublico_BloqueaDestinosInternos(t *testing.T) {
	destinos := []struct {
		url    string
		motivo string
	}{
		{"http://169.254.169.254/latest/meta-data/", "metadatos de la nube (entrega credenciales)"},
		{"http://127.0.0.1:6379/", "Redis local"},
		{"http://localhost:5432/", "Postgres local"},
		{"http://10.0.0.5/admin", "red privada 10.0.0.0/8"},
		{"http://192.168.1.1/", "router de la red local"},
		{"http://172.17.0.2:3001/", "red interna de Docker"},
		{"http://[::1]:6379/", "loopback IPv6"},
		{"http://100.64.0.1/", "CGNAT"},
		{"http://0.0.0.0/", "dirección sin especificar"},
	}
	c := ClientePublico(3 * time.Second)
	for _, d := range destinos {
		t.Run(d.motivo, func(t *testing.T) {
			_, err := c.Get(d.url)
			if err == nil {
				t.Fatalf("se permitió llegar a %s (%s)", d.url, d.motivo)
			}
			if !errors.Is(err, ErrDestinoInterno) {
				t.Fatalf("se bloqueó por otro motivo (%v); debe ser ErrDestinoInterno", err)
			}
		})
	}
}

// Un servidor legítimo no debe verse afectado por la protección.
func TestClientePublico_PermiteDireccionesPublicas(t *testing.T) {
	casos := map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "93.184.216.34": true,
		"127.0.0.1": false, "10.1.2.3": false, "169.254.169.254": false,
		"172.16.0.1": false, "192.168.0.1": false,
	}
	for s, want := range casos {
		if got := esIPPublica(net.ParseIP(s)); got != want {
			t.Errorf("esIPPublica(%s)=%v, quería %v", s, got, want)
		}
	}
}

// Un redirect no debe seguirse solo: un servidor externo podría responder
// "302 → http://169.254.169.254/" y usar nuestra petición como puente.
func TestClientePublico_NoSigueRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/salto" {
			http.Redirect(w, r, "http://169.254.169.254/latest/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Se usa el transporte normal para poder llegar al servidor de prueba (que
	// está en loopback); lo que se comprueba es la política de redirects.
	c := ClientePublico(3 * time.Second)
	c.Transport = guard{next: transport()}

	resp, err := c.Get(srv.URL + "/salto")
	if err != nil {
		t.Fatalf("error inesperado: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("se esperaba que devolviera el 302 sin seguirlo, dio %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Fatal("debería devolver el Location para que decida quien llama")
	}
}

// Package secnet centraliza la creación de clientes HTTP SALIENTES endurecidos
// contra ataques man-in-the-middle (MitM).
//
// Por qué existe: antes cada paquete creaba su propio `&http.Client{Timeout: X}`.
// Eso hereda el transporte por defecto de Go, que es razonable pero deja tres
// puertas abiertas:
//
//  1. Si alguien configura una URL con `http://` (p. ej. BTCPAY_URL o
//     NVIDIA_BASE_URL), el tráfico —con API keys dentro— sale EN CLARO y
//     cualquiera en la ruta puede leerlo o modificarlo. Nadie se entera.
//  2. Un servidor puede responder con un redirect de `https://` a `http://`
//     y el cliente de Go lo sigue alegremente: degradación silenciosa a texto plano.
//  3. No hay versión mínima de TLS explícita, así que un futuro cambio de
//     `InsecureSkipVerify` o de MinVersion pasaría desapercibido en review.
//
// Este paquete cierra las tres: exige TLS >= 1.2 con verificación de certificado
// SIEMPRE activa, prohíbe el texto plano hacia hosts públicos y rechaza los
// redirects que degraden https→http.
package secnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrPlaintext se devuelve cuando se intenta hablar http:// (sin TLS) con un
// host que NO es local/privado. Es un fallo de configuración, no de red: la
// petición no se envía, para que un secreto nunca viaje en claro por internet.
var ErrPlaintext = errors.New("secnet: se bloqueó una petición HTTP en texto plano hacia un host público (usa https://)")

// ErrDowngrade se devuelve cuando un redirect intenta bajar de https a http.
var ErrDowngrade = errors.New("secnet: redirect bloqueado por degradar HTTPS a HTTP en texto plano")

// baseTLS es la configuración TLS de todos los clientes salientes.
//
// InsecureSkipVerify se deja escrito EXPLÍCITAMENTE en false: es el valor por
// defecto, pero dejarlo visible convierte cualquier intento futuro de
// desactivar la verificación en un cambio evidente durante la revisión de código.
func baseTLS() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false,
	}
}

// Transport construye el transporte endurecido.
func transport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: baseTLS(),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Corta si el servidor acepta la conexión pero no manda cabeceras: evita
		// que un intermediario deje peticiones colgadas consumiendo recursos.
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

// guard envuelve el transporte para bloquear el texto plano hacia hosts públicos.
type guard struct{ next http.RoundTripper }

func (g guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "http" && !localOrPrivate(r.Context(), r.URL.Hostname()) {
		return nil, fmt.Errorf("%w: %s", ErrPlaintext, r.URL.Host)
	}
	return g.next.RoundTrip(r)
}

// Client devuelve un http.Client endurecido con el timeout total indicado.
//
// El texto plano SOLO se permite hacia loopback o redes privadas (RFC1918), que
// es donde viven los servicios internos: Ollama en `http://ollama:11434` dentro
// de la red de Docker, o `http://localhost:11434` en desarrollo. Ese tráfico no
// sale de la máquina/red del VPS, así que no es interceptable desde fuera.
// Cualquier http:// hacia una IP pública se bloquea.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: guard{next: transport()},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("secnet: demasiados redirects")
			}
			// Degradación https -> http: es exactamente la maniobra que usa un
			// atacante en ruta para poder leer la petición siguiente.
			if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme == "http" {
				return fmt.Errorf("%w: %s -> %s", ErrDowngrade, via[len(via)-1].URL.Host, req.URL.Host)
			}
			return nil
		},
	}
}

// --- resolución de "host local o privado" con caché corta ---

type cachedResult struct {
	private bool
	at      time.Time
}

var (
	resolveMu    sync.RWMutex
	resolveCache = map[string]cachedResult{}
)

const resolveTTL = 60 * time.Second

// localOrPrivate indica si el host es loopback o resuelve ÚNICAMENTE a IPs
// privadas/loopback. Si una sola de sus IPs es pública, devuelve false: así un
// DNS que mezcle una IP interna con una externa no sirve para colar texto plano.
func localOrPrivate(ctx context.Context, host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}

	resolveMu.RLock()
	c, ok := resolveCache[host]
	resolveMu.RUnlock()
	if ok && time.Since(c.at) < resolveTTL {
		return c.private
	}

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	private := err == nil && len(ips) > 0
	if private {
		for _, ia := range ips {
			if !isPrivateIP(ia.IP) {
				private = false
				break
			}
		}
	}

	resolveMu.Lock()
	resolveCache[host] = cachedResult{private: private, at: time.Now()}
	resolveMu.Unlock()
	return private
}

// isPrivateIP cubre loopback, link-local, RFC1918 y las ULA de IPv6 (fc00::/7),
// que son las redes en las que viven los contenedores de Docker.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// ServerTLSConfig es la configuración TLS para el servidor HTTPS del propio API
// (cuando se sirve TLS de forma nativa, sin proxy delante).
//
// TLS 1.2 como mínimo y solo suites con forward secrecy y AEAD: quedan fuera
// RC4, 3DES, CBC y el intercambio de clave RSA estático, que es lo que permite
// descifrar tráfico capturado si la clave privada se filtra más adelante.
func ServerTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{
			tls.X25519, tls.CurveP256, tls.CurveP384,
		},
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}
}

// --- Cliente para URLs que suministra el usuario (anti-SSRF) ----------------

// ErrDestinoInterno se devuelve cuando una URL suministrada por un usuario
// apunta a la red interna. Es la defensa contra SSRF (Server-Side Request
// Forgery): sin ella, quien pueda hacer que el servidor descargue una URL puede
// usarlo como puente hacia sitios a los que él no llega —el Postgres y el Redis
// de la red interna de Docker, o el servicio de metadatos del proveedor de nube
// en 169.254.169.254, que entrega credenciales de la máquina sin pedir nada.
var ErrDestinoInterno = errors.New("secnet: la URL apunta a una dirección interna y se bloqueó (SSRF)")

// ClientePublico devuelve un cliente para descargar URLs que ha escrito un
// usuario. Solo permite salir a direcciones públicas de internet.
//
// La comprobación se hace en el momento de CONECTAR, no al mirar la URL. Es
// deliberado: si se resolviera el nombre antes y se conectara después, un
// atacante puede hacer que su dominio devuelva una IP pública en la primera
// consulta y una interna en la segunda (DNS rebinding), y la validación previa
// no serviría de nada. Comprobando la IP a la que realmente se está abriendo el
// socket, esa ventana no existe.
func ClientePublico(timeout time.Duration) *http.Client {
	tr := transport()
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("%w: %s", ErrDestinoInterno, addr)
		}
		if !esIPPublica(ip) {
			return nil, fmt.Errorf("%w: %s", ErrDestinoInterno, ip)
		}
		return base.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		// Sin redirects: un servidor externo podría responder "302 →
		// http://169.254.169.254/" y usar nuestra propia petición para saltar a
		// la red interna. Quien llama decide si sigue el redirect a mano.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// esIPPublica descarta todo lo que no sea internet abierto: loopback, redes
// privadas, link-local (incluido 169.254.169.254, el servicio de metadatos de
// las nubes), multicast y las direcciones reservadas.
func esIPPublica(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // 0.0.0.0/8
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // CGNAT 100.64.0.0/10
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // IETF 192.0.0.0/24
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // benchmarking
			return false
		case v4[0] >= 240: // reservado / broadcast
			return false
		}
		return true
	}
	// IPv6: descarta ULA (fc00::/7) y las direcciones IPv4 embebidas.
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return false
	}
	return true
}

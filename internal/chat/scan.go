package chat

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

// Estados del escaneo antimalware de un adjunto.
const (
	scanClean    = "clean"     // ClamAV lo revisó y está limpio
	scanInfected = "infected"  // ClamAV detectó una firma de malware
	scanError    = "error"     // no se pudo escanear (clamd caído/timeout)
	scanDisabled = "unscanned" // CLAMAV_ADDR no configurado
)

type scanVerdict struct {
	Status    string
	Signature string
}

// scanWithClamd escanea data con el protocolo INSTREAM de clamd. addr admite
// "host:port" (TCP) o "unix:/ruta/al/socket". NUNCA bloquea el flujo de subida:
// si falla, el llamador marca el adjunto como "error" (fail-open) y el archivo
// se guarda igual, solo que sin verdicto limpio.
func scanWithClamd(ctx context.Context, addr string, data []byte) (scanVerdict, error) {
	network, address := "tcp", addr
	if strings.HasPrefix(addr, "unix:") {
		network, address = "unix", strings.TrimPrefix(addr, "unix:")
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return scanVerdict{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return scanVerdict{}, err
	}
	const chunk = 64 * 1024
	hdr := make([]byte, 4)
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		binary.BigEndian.PutUint32(hdr, uint32(end-i))
		if _, err := conn.Write(hdr); err != nil {
			return scanVerdict{}, err
		}
		if _, err := conn.Write(data[i:end]); err != nil {
			return scanVerdict{}, err
		}
	}
	binary.BigEndian.PutUint32(hdr, 0) // terminador de stream
	if _, err := conn.Write(hdr); err != nil {
		return scanVerdict{}, err
	}

	resp, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return scanVerdict{}, err
	}
	resp = strings.TrimRight(resp, "\x00\n ")
	// Respuestas: "stream: OK" | "stream: <FIRMA> FOUND" | "... ERROR"
	switch {
	case strings.HasSuffix(resp, "OK"):
		return scanVerdict{Status: scanClean}, nil
	case strings.HasSuffix(resp, "FOUND"):
		sig := strings.TrimSpace(strings.TrimSuffix(resp, "FOUND"))
		sig = strings.TrimSpace(strings.TrimPrefix(sig, "stream:"))
		return scanVerdict{Status: scanInfected, Signature: sig}, nil
	default:
		return scanVerdict{}, errors.New("clamd: respuesta inesperada: " + resp)
	}
}

// scanAttachment aplica el escaneo si CLAMAV_ADDR está configurado. Devuelve el
// estado ("clean"/"infected"/"error"/"unscanned") y, si aplica, la firma.
func (s *Service) scanAttachment(ctx context.Context, data []byte) (string, string) {
	addr := strings.TrimSpace(s.cfg.ClamAVAddr)
	if addr == "" {
		return scanDisabled, ""
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	v, err := scanWithClamd(cctx, addr, data)
	if err != nil {
		log.Printf("[chat] escaneo ClamAV falló (se marca 'error', el archivo se guarda igual): %v", err)
		return scanError, ""
	}
	return v.Status, v.Signature
}

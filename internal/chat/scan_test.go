package chat

import (
	"context"
	"net"
	"testing"
	"time"
)

// fakeClamd levanta un servidor TCP que responde como clamd INSTREAM: lee el
// comando + los chunks y responde con la línea indicada.
func fakeClamd(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		// Lee hasta el terminador de stream (4 bytes cero) de forma simplona.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			n, err := conn.Read(buf)
			if n > 0 && containsZeroTerminator(buf[:n]) {
				break
			}
			if err != nil {
				break
			}
		}
		_, _ = conn.Write([]byte(reply + "\x00"))
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func containsZeroTerminator(b []byte) bool {
	// heurística de test: el último chunk termina con 4 ceros
	if len(b) < 4 {
		return false
	}
	tail := b[len(b)-4:]
	return tail[0] == 0 && tail[1] == 0 && tail[2] == 0 && tail[3] == 0
}

func TestScanCleanAndInfected(t *testing.T) {
	ctx := context.Background()

	addrOK := fakeClamd(t, "stream: OK")
	v, err := scanWithClamd(ctx, addrOK, []byte("contenido limpio"))
	if err != nil {
		t.Fatalf("scan OK error: %v", err)
	}
	if v.Status != scanClean {
		t.Fatalf("esperaba clean, obtuve %q", v.Status)
	}

	addrBad := fakeClamd(t, "stream: Eicar-Test-Signature FOUND")
	v, err = scanWithClamd(ctx, addrBad, []byte("X5O!P%@AP..."))
	if err != nil {
		t.Fatalf("scan FOUND error: %v", err)
	}
	if v.Status != scanInfected {
		t.Fatalf("esperaba infected, obtuve %q", v.Status)
	}
	if v.Signature != "Eicar-Test-Signature" {
		t.Fatalf("firma inesperada: %q", v.Signature)
	}
}

func TestScanDialError(t *testing.T) {
	// Puerto donde no hay nada escuchando → error (fail-open en el llamador).
	_, err := scanWithClamd(context.Background(), "127.0.0.1:1", []byte("x"))
	if err == nil {
		t.Fatal("esperaba error de conexión")
	}
}

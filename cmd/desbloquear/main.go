// Command desbloquear quita el bloqueo por fuerza bruta de una cuenta.
//
// Existe como herramienta de línea de comandos y NO como endpoint HTTP, y esa
// decisión es deliberada: una ruta que informe de cuántas veces se ha intentado
// entrar en una cuenta es, para quien logre alcanzarla, un panel con el progreso
// de su propio ataque; y una que desbloquee es una forma de anular la defensa.
// Ninguna de las dos debe existir en la superficie de red.
//
// Aquí hace falta acceso al servidor y a Redis, que es exactamente el nivel de
// permiso que debe tener quien desbloquea una cuenta.
//
// Por qué se necesita: el bloqueo es por cuenta, así que cualquiera que conozca
// un email puede fallar adrede para dejar a esa persona fuera. El bloqueo caduca
// solo (una hora como mucho), pero esto devuelve el acceso al momento.
//
// Uso:
//
//	go run ./cmd/desbloquear correo@ejemplo.com
//	go run ./cmd/desbloquear -estado correo@ejemplo.com   # solo consultar
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ambitos son los contadores que mantiene cada endpoint de credenciales.
var ambitos = []string{
	"index",    // POST /api/index          — login con PIN de la app
	"login",    // POST /api/login          — email + clave
	"activar",  // POST /api/activar        — canje de código
	"consumir", // POST /api/consumirCode   — consumo de código de vendedor
	"session",  // POST /api/chat/session
	"support",  // POST /api/chat/support/*
	"admin",    // POST /api/chat/admin/session
}

func main() {
	soloEstado := flag.Bool("estado", false, "solo consultar, sin desbloquear")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Uso: desbloquear [-estado] <email>")
		os.Exit(2)
	}
	cuenta := strings.ToLower(strings.TrimSpace(flag.Arg(0)))
	if cuenta == "" {
		fmt.Fprintln(os.Stderr, "El email no puede estar vacío")
		os.Exit(2)
	}

	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REDIS_URL inválida: %v\n", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "No se pudo conectar a Redis: %v\n", err)
		fmt.Fprintln(os.Stderr, "Comprueba REDIS_URL (incluye la contraseña) y que el contenedor esté arriba.")
		os.Exit(1)
	}

	fmt.Printf("Cuenta: %s\n\n", cuenta)
	totalFallos, bloqueada := 0, false

	for _, a := range ambitos {
		claveFallos := "fail:" + a + ":" + cuenta
		claveBloqueo := "lock:" + a + ":" + cuenta

		fallos, _ := rdb.Get(ctx, claveFallos).Int()
		ttl, _ := rdb.TTL(ctx, claveBloqueo).Result()

		if fallos == 0 && ttl <= 0 {
			continue
		}
		totalFallos += fallos
		estado := "sin bloquear"
		if ttl > 0 {
			bloqueada = true
			estado = fmt.Sprintf("BLOQUEADA %s más", ttl.Round(1e9))
		}
		fmt.Printf("  %-10s %d fallo(s), %s\n", a, fallos, estado)

		if !*soloEstado {
			_ = rdb.Del(ctx, claveFallos, claveBloqueo).Err()
		}
	}

	if totalFallos == 0 && !bloqueada {
		fmt.Println("  Sin fallos registrados. No hay nada que desbloquear.")
		return
	}
	if *soloEstado {
		fmt.Printf("\nTotal: %d fallo(s). Para desbloquear, repite sin -estado.\n", totalFallos)
		return
	}
	fmt.Printf("\n✓ Desbloqueada. Se borraron los contadores (%d fallo(s) en total).\n", totalFallos)
}

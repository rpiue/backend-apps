// Command genaccesskey genera un par de claves Ed25519 para firmar los tokens
// de acceso/suscripción.
//
//	go run ./backend/cmd/genaccesskey
//
// Imprime:
//   - ACCESS_SIGNING_KEY: semilla privada (base64). Ponla como variable de
//     entorno en el SERVIDOR. NUNCA la incluyas en la app ni la subas al repo.
//   - PUBLIC_KEY: clave pública (base64). Embébela en la app Flutter para
//     verificar las firmas offline.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	seed := priv.Seed() // 32 bytes
	fmt.Println("ACCESS_SIGNING_KEY (servidor, secreta):")
	fmt.Println("  " + base64.StdEncoding.EncodeToString(seed))
	fmt.Println()
	fmt.Println("PUBLIC_KEY (embeber en la app):")
	fmt.Println("  " + base64.StdEncoding.EncodeToString(pub))
}

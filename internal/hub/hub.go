// Package hub es un bus de WebSocket simple: los clientes del panel admin se
// conectan y reciben en tiempo real los eventos del chat (mensajes nuevos).
package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	secmw "codex/backend/internal/middleware"
)

// upgrader valida el Origin del upgrade. Por defecto acepta todo (compatibilidad
// con clientes nativos y con instalaciones sin CORS_ORIGINS); main.go lo restringe
// a los orígenes configurados llamando a SetAllowedOrigins al arrancar.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// SetAllowedOrigins restringe qué orígenes de navegador pueden abrir el
// WebSocket del panel. Se llama una vez al arrancar, antes de servir tráfico.
func SetAllowedOrigins(origins []string) {
	upgrader.CheckOrigin = secmw.OriginChecker(origins)
}

// Event es lo que se difunde a los paneles conectados.
type Event struct {
	Type string `json:"type"` // "mensaje" | "conversacion"
	Data any    `json:"data"`
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

func New() *Hub { return &Hub{clients: make(map[*websocket.Conn]bool)} }

// Handle actualiza la conexión a WebSocket y la registra.
func (h *Hub) Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()

	// Lectura para detectar cierre (descartamos el contenido entrante).
	go func() {
		defer h.remove(conn)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (h *Hub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
	_ = conn.Close()
}

// Broadcast envía un evento a todos los paneles conectados.
func (h *Hub) Broadcast(ev Event) {
	b, _ := json.Marshal(ev)
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
			h.remove(c)
		}
	}
}

package chat

import (
	"encoding/json"
	"errors"
	"net/http"
)

// readBody decodifica el JSON del request. En error responde 400 y devuelve false.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeErr(w, http.StatusBadRequest, "Cuerpo inválido")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "Cuerpo inválido")
		return false
	}
	return true
}

// writeChatErr responde usando el statusCode del chatErr (o 500 genérico).
func writeChatErr(w http.ResponseWriter, err error) {
	var ce chatErr
	if errors.As(err, &ce) {
		writeJSON(w, ce.status, map[string]any{"ok": false, "error": ce.msg})
		return
	}
	writeErr(w, http.StatusInternalServerError, "Error interno del chat")
}

// messageBody es el cuerpo aceptado por /messages y /support/message.
type messageBody struct {
	Encrypted        *Encrypted `json:"encrypted"`
	Text             string     `json:"text"`
	Message          string     `json:"message"`
	Mensaje          string     `json:"mensaje"`
	ClientNonce      string     `json:"clientNonce"`
	ReplyToMessageID *int64     `json:"replyToMessageId"`
	ReplyToSnake     *int64     `json:"reply_to_message_id"`
	ReplyTo          *int64     `json:"replyTo"`
	ConversationID   int64      `json:"conversationId"`
	// extras de /session y /support/*
	App      string `json:"app"`
	Email    string `json:"email"`
	Pin      string `json:"pin"`
	Clave    string `json:"clave"`
	DeviceID string `json:"deviceId"`
}

func (b messageBody) textValue() string {
	if b.Text != "" {
		return b.Text
	}
	if b.Message != "" {
		return b.Message
	}
	return b.Mensaje
}

func (b messageBody) replyToID() *int64 {
	for _, v := range []*int64{b.ReplyToMessageID, b.ReplyToSnake, b.ReplyTo} {
		if v != nil && *v > 0 {
			return v
		}
	}
	return nil
}

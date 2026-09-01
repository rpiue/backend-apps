package notify

import (
	"context"
	"log"
)

// LogTransport es el transporte de Fase 1: registra en consola en lugar de
// hablar con FCM. En Fase 2 se sustituye por FCMTransport (Firebase Admin SDK),
// sin cambiar nada del resto del código gracias a la interfaz Transport.
type LogTransport struct{}

func (LogTransport) SendToTopic(_ context.Context, topic string, msg Message) error {
	log.Printf("[notify] -> topic=%s title=%q body=%q mode=%s", topic, msg.Title, msg.Body, msg.Mode)
	return nil
}

func (LogTransport) SendToToken(_ context.Context, token string, msg Message) error {
	log.Printf("[notify] -> token=%.12s… title=%q body=%q mode=%s", token, msg.Title, msg.Body, msg.Mode)
	return nil
}

func (LogTransport) SubscribeTopic(_ context.Context, tokens []string, topic string) error {
	log.Printf("[notify] subscribe %d tokens -> %s", len(tokens), topic)
	return nil
}

func (LogTransport) UnsubscribeTopic(_ context.Context, tokens []string, topic string) error {
	log.Printf("[notify] unsubscribe %d tokens -> %s", len(tokens), topic)
	return nil
}

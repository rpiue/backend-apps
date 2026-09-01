package ai

import (
	"sync"
	"time"
)

// Debouncer agrupa eventos por clave: cada llamada a Trigger reinicia un
// temporizador; cuando pasan `delay` sin nuevos Trigger para esa clave, ejecuta fn.
//
// Es justo lo que pide el chat: el usuario manda un mensaje → esperar 5s; si
// vuelve a escribir, se reinicia el contador; cuando deja de escribir 5s, responde.
type Debouncer struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
	delay  time.Duration
}

func NewDebouncer(delay time.Duration) *Debouncer {
	return &Debouncer{timers: make(map[string]*time.Timer), delay: delay}
}

// Trigger (re)programa la ejecución de fn para `key` dentro de `delay`.
func (d *Debouncer) Trigger(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	d.timers[key] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		delete(d.timers, key)
		d.mu.Unlock()
		fn()
	})
}

// Cancel detiene el temporizador pendiente de una clave (si lo hay).
func (d *Debouncer) Cancel(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Stop()
		delete(d.timers, key)
	}
}

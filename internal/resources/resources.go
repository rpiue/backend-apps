// Package resources mantiene los banners y anuncios (publicidad) que /datosApp
// devuelve para "yape". Replica las listas en memoria de yape.js. La función
// Refresh (fase 3, cron) los actualizará desde la API externa.
package resources

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed banners.json
var bannersSeed []byte

//go:embed anuncios.json
var anunciosSeed []byte

type Store struct {
	mu       sync.RWMutex
	banners  []map[string]any
	anuncios []map[string]any
}

func New() *Store {
	s := &Store{}
	_ = json.Unmarshal(bannersSeed, &s.banners)
	_ = json.Unmarshal(anunciosSeed, &s.anuncios)
	return s
}

func (s *Store) Banners() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.banners
}

func (s *Store) Anuncios() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.anuncios
}

// Set actualiza ambas listas (lo usará el refresher en fase 3).
func (s *Store) Set(banners, anuncios []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if banners != nil {
		s.banners = banners
	}
	if anuncios != nil {
		s.anuncios = anuncios
	}
}

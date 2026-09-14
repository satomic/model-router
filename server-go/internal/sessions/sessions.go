// Package sessions holds session stickiness: an in-memory TTL + LRU store of
// session id -> chosen model.
package sessions

import (
	"container/list"
	"sync"
	"time"
)

type entry struct {
	key     string
	model   string
	expires time.Time
}

type Store struct {
	ttl         time.Duration
	maxSessions int
	mu          sync.Mutex
	order       *list.List
	items       map[string]*list.Element
}

func New(ttlSeconds, maxSessions int) *Store {
	return &Store{
		ttl:         time.Duration(ttlSeconds) * time.Second,
		maxSessions: maxSessions,
		order:       list.New(),
		items:       map[string]*list.Element{},
	}
}

func (s *Store) Get(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.items[sessionID]
	if !ok {
		return ""
	}
	item := element.Value.(*entry)
	if time.Now().After(item.expires) {
		s.order.Remove(element)
		delete(s.items, sessionID)
		return ""
	}
	s.order.MoveToBack(element)
	return item.model
}

func (s *Store) Set(sessionID, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.items[sessionID]; ok {
		item := element.Value.(*entry)
		item.model = model
		item.expires = time.Now().Add(s.ttl)
		s.order.MoveToBack(element)
		return
	}
	element := s.order.PushBack(&entry{key: sessionID, model: model, expires: time.Now().Add(s.ttl)})
	s.items[sessionID] = element
	for s.order.Len() > s.maxSessions {
		oldest := s.order.Front()
		if oldest == nil {
			break
		}
		s.order.Remove(oldest)
		delete(s.items, oldest.Value.(*entry).key)
	}
}

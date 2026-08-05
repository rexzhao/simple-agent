package wsgateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTicketTTL       = 30 * time.Second
	DefaultMaxTickets      = 4096
	defaultTicketByteCount = 32
	maxTicketIssueAttempts = 8
)

var (
	ErrTicketInvalid   = errors.New("invalid websocket ticket")
	ErrTicketStoreFull = errors.New("websocket ticket store is full")
	ErrTicketCollision = errors.New("websocket ticket generation collided")
)

// TicketClaims are the non-secret facts associated with a ticket. The raw
// ticket is never retained by TicketStore.
type TicketClaims struct {
	Principal string
}

type TicketStoreOptions struct {
	TTL        time.Duration
	MaxTickets int
	Now        func() time.Time
	Random     io.Reader
}

type ticketRecord struct {
	claims    TicketClaims
	expiresAt time.Time
}

// TicketStore issues short-lived, single-use tickets. It stores only a SHA-256
// digest of each ticket, so a memory disclosure does not expose usable URLs.
type TicketStore struct {
	mu         sync.Mutex
	tickets    map[[sha256.Size]byte]ticketRecord
	ttl        time.Duration
	maxTickets int
	now        func() time.Time
	random     io.Reader
}

func NewTicketStore(options TicketStoreOptions) (*TicketStore, error) {
	ttl := options.TTL
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	maxTickets := options.MaxTickets
	if maxTickets <= 0 {
		maxTickets = DefaultMaxTickets
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	return &TicketStore{
		tickets:    make(map[[sha256.Size]byte]ticketRecord),
		ttl:        ttl,
		maxTickets: maxTickets,
		now:        now,
		random:     random,
	}, nil
}

// Issue creates a URL-safe opaque ticket and returns its expiry. Only the
// caller receives the raw value; the store keeps its digest.
func (s *TicketStore) Issue(claims TicketClaims) (string, time.Time, error) {
	if s == nil {
		return "", time.Time{}, ErrTicketInvalid
	}
	now := s.now()
	expiresAt := now.Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(now)
	if len(s.tickets) >= s.maxTickets {
		return "", time.Time{}, ErrTicketStoreFull
	}

	// Keep entropy generation and collision detection under the same lock as
	// insertion. A deterministic/test or malfunctioning random source must not
	// overwrite a still-valid record belonging to an earlier caller.
	for attempt := 0; attempt < maxTicketIssueAttempts; attempt++ {
		raw := make([]byte, defaultTicketByteCount)
		if _, err := io.ReadFull(s.random, raw); err != nil {
			return "", time.Time{}, fmt.Errorf("generate websocket ticket: %w", err)
		}
		ticket := base64.RawURLEncoding.EncodeToString(raw)
		digest := sha256.Sum256([]byte(ticket))
		if _, exists := s.tickets[digest]; exists {
			continue
		}
		s.tickets[digest] = ticketRecord{claims: claims, expiresAt: expiresAt}
		return ticket, expiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("%w after %d attempts", ErrTicketCollision, maxTicketIssueAttempts)
}

// Consume validates and atomically removes a ticket. A ticket can therefore
// succeed in at most one concurrent Upgrade attempt, including attempts made
// after its expiry.
func (s *TicketStore) Consume(ticket string) (TicketClaims, bool) {
	if s == nil {
		return TicketClaims{}, false
	}
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return TicketClaims{}, false
	}
	digest := sha256.Sum256([]byte(ticket))
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.tickets[digest]
	if !ok {
		return TicketClaims{}, false
	}
	// Delete before checking the deadline: expired values are consumed too and
	// can never be retried if the clock is adjusted or a request races expiry.
	delete(s.tickets, digest)
	if !now.Before(record.expiresAt) {
		return TicketClaims{}, false
	}
	return record.claims, true
}

func (s *TicketStore) removeExpiredLocked(now time.Time) {
	for digest, record := range s.tickets {
		if !now.Before(record.expiresAt) {
			delete(s.tickets, digest)
		}
	}
}

func (s *TicketStore) Size() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(s.now())
	return len(s.tickets)
}

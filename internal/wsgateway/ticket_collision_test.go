package wsgateway

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestTicketStoreCollisionRetriesWithoutOverwritingValidRecord(t *testing.T) {
	firstBytes := bytes.Repeat([]byte{'a'}, defaultTicketByteCount)
	secondBytes := bytes.Repeat([]byte{'b'}, defaultTicketByteCount)
	random := bytes.NewReader(append(append(append([]byte(nil), firstBytes...), firstBytes...), secondBytes...))
	now := time.Unix(20, 0)
	store, err := NewTicketStore(TicketStoreOptions{Random: random, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Issue(TicketClaims{Principal: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.Issue(TicketClaims{Principal: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("collision retry returned the existing ticket")
	}
	firstClaims, ok := store.Consume(first)
	if !ok || firstClaims.Principal != "first" {
		t.Fatalf("first ticket claims=%#v/%v, want first", firstClaims, ok)
	}
	secondClaims, ok := store.Consume(second)
	if !ok || secondClaims.Principal != "second" {
		t.Fatalf("second ticket claims=%#v/%v, want second", secondClaims, ok)
	}
}

func TestTicketStoreCollisionExhaustionPreservesExistingTicket(t *testing.T) {
	repeated := bytes.Repeat([]byte{'x'}, defaultTicketByteCount*(1+maxTicketIssueAttempts))
	store, err := NewTicketStore(TicketStoreOptions{Random: bytes.NewReader(repeated)})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Issue(TicketClaims{Principal: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Issue(TicketClaims{Principal: "replacement"}); !errors.Is(err, ErrTicketCollision) {
		t.Fatalf("collision exhaustion error=%v, want ErrTicketCollision", err)
	}
	claims, ok := store.Consume(first)
	if !ok || claims.Principal != "original" {
		t.Fatalf("original ticket claims=%#v/%v after collision", claims, ok)
	}
}

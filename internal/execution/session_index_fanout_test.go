package execution

import (
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessionindex"
)

type fanoutRecordingSink struct {
	mu      sync.Mutex
	changes []sessionindex.CommittedChange
}

func (s *fanoutRecordingSink) PublishCommitted(change sessionindex.CommittedChange) error {
	s.mu.Lock()
	s.changes = append(s.changes, change)
	s.mu.Unlock()
	return nil
}

func (s *fanoutRecordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.changes)
}

func TestSessionIndexSinkFanoutAndOwnerUnregister(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstSink := &fanoutRecordingSink{}
	secondSink := &fanoutRecordingSink{}
	first := service.RegisterSessionIndexChangeSink(firstSink)
	second := service.RegisterSessionIndexChangeSink(secondSink)
	change := sessionindex.CommittedChange{Kind: sessionindex.CommittedProjectRefresh, ProjectID: "p"}
	service.publishSessionIndexChange(change)
	if firstSink.count() != 1 || secondSink.count() != 1 {
		t.Fatalf("initial fanout first=%d second=%d", firstSink.count(), secondSink.count())
	}
	first.Unregister()
	service.publishSessionIndexChange(change)
	if firstSink.count() != 1 || secondSink.count() != 2 {
		t.Fatalf("owner unregister first=%d second=%d", firstSink.count(), secondSink.count())
	}
	second.Unregister()
}

func TestSessionIndexSinkRegisterUnregisterPublishRaceKeepsRegisteredSink(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stable := &fanoutRecordingSink{}
	stableRegistration := service.RegisterSessionIndexChangeSink(stable)
	defer stableRegistration.Unregister()
	const publishes = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < publishes; i++ {
			service.publishSessionIndexChange(sessionindex.CommittedChange{Kind: sessionindex.CommittedProjectRefresh, ProjectID: "p"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			temporary := service.RegisterSessionIndexChangeSink(&fanoutRecordingSink{})
			temporary.Unregister()
		}
	}()
	wg.Wait()
	if got := stable.count(); got != publishes {
		t.Fatalf("stable registration lost publishes: got=%d want=%d", got, publishes)
	}
}

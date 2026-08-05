package execution

import (
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessionindex"
)

type registrationSink struct{}

func (registrationSink) PublishCommitted(sessionindex.CommittedChange) error { return nil }

func TestSessionIndexSinkUnregisterDoesNotDetachNewOwner(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := service.RegisterSessionIndexChangeSink(registrationSink{})
	second := service.RegisterSessionIndexChangeSink(registrationSink{})
	first.Unregister()
	if service.sessionIndexChangeSink() == nil {
		t.Fatal("old registration detached newer sink")
	}
	second.Unregister()
	if service.sessionIndexChangeSink() != nil {
		t.Fatal("current registration was not detached")
	}
}

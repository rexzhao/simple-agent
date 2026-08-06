package wsgateway

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func issueTicketForPrincipal(t *testing.T, endpoint *testEndpoint, principal string) string {
	t.Helper()
	ticket, _, err := endpoint.store.Issue(TicketClaims{Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func commandMessage(id, requestID, name string, arguments string) protocol.CommandMessage {
	return protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: id},
		Payload:  protocol.CommandPayload{Name: name, SchemaVersion: 1, RequestID: requestID, Arguments: json.RawMessage(arguments)},
	}
}

func readCommandResult(t *testing.T, connection *websocket.Conn) protocol.CommandResultMessage {
	t.Helper()
	message := readProtocol(t, connection)
	result, ok := message.(protocol.CommandResultMessage)
	if !ok {
		t.Fatalf("message=%#v, want command_result", message)
	}
	return result
}

func TestDispatcherRejectsUnsupportedExpectedRevisionBeforeExecution(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	var executions atomic.Int64
	definition := commands.CommandDefinition{Name: "fake.no-revision", SchemaVersion: 1, Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"ok":true}`), nil
	}}
	endpoint, _ := newDispatcherForTest(t, provider, definition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "revision-client")
	_ = readProtocol(t, connection)
	revision := protocol.ResourceRevision("7")
	message := commandMessage("revision-command", "revision-request", "fake.no-revision", `{}`)
	message.Payload.ExpectedRevision = &revision
	writeProtocol(t, connection, message)
	result := readCommandResult(t, connection)
	if result.Payload.Status != protocol.CommandStatusFailed || result.Payload.Error == nil || result.Payload.Error.Code != "unsupported_expected_revision" {
		t.Fatalf("unsupported revision result=%#v", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("command executed despite unsupported revision: %d", executions.Load())
	}
}

func TestDispatcherCommandCacheIsPrincipalScopedAcrossRunningAndCompleted(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	started := make(chan string, 2)
	release := make(chan struct{})
	var executions atomic.Int64
	definition := commands.CommandDefinition{Name: "fake.principal", SchemaVersion: 1, Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
		executions.Add(1)
		started <- request.Principal
		select {
		case <-release:
			return json.Marshal(map[string]string{"principal": request.Principal})
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	endpoint, _ := newDispatcherForTest(t, provider, definition)
	one := dialTest(t, endpoint, issueTicketForPrincipal(t, endpoint, "principal-A"))
	defer one.Close(websocket.StatusNormalClosure, "done")
	two := dialTest(t, endpoint, issueTicketForPrincipal(t, endpoint, "principal-B"))
	defer two.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, one, "principal-a-client")
	_ = readProtocol(t, one)
	writeHello(t, two, "principal-b-client")
	_ = readProtocol(t, two)
	writeProtocol(t, one, commandMessage("command-a", "same-request", "fake.principal", `{"value":1}`))
	writeProtocol(t, two, commandMessage("command-b", "same-request", "fake.principal", `{"value":1}`))
	if message := readProtocol(t, one); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("A accepted=%s", message.Kind())
	}
	if message := readProtocol(t, two); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("B accepted=%s", message.Kind())
	}
	seen := map[string]bool{}
	seen[<-started] = true
	seen[<-started] = true
	if !seen["principal-A"] || !seen["principal-B"] {
		t.Fatalf("executions principals=%v", seen)
	}
	close(release)
	first := readCommandResult(t, one)
	second := readCommandResult(t, two)
	if string(first.Payload.Result) != `{"principal":"principal-A"}` || string(second.Payload.Result) != `{"principal":"principal-B"}` {
		t.Fatalf("cross-principal results=%#v/%#v", first, second)
	}
	if executions.Load() != 2 {
		t.Fatalf("executions=%d, want 2", executions.Load())
	}
	// Completed cache entries remain independently reusable without accepting
	// or executing a new side effect.
	writeProtocol(t, one, commandMessage("retry-a", "same-request", "fake.principal", `{"value":1}`))
	writeProtocol(t, two, commandMessage("retry-b", "same-request", "fake.principal", `{"value":1}`))
	if got := readCommandResult(t, one); string(got.Payload.Result) != `{"principal":"principal-A"}` {
		t.Fatalf("A cached result=%s", got.Payload.Result)
	}
	if got := readCommandResult(t, two); string(got.Payload.Result) != `{"principal":"principal-B"}` {
		t.Fatalf("B cached result=%s", got.Payload.Result)
	}
	if executions.Load() != 2 {
		t.Fatalf("cached retries executed: %d", executions.Load())
	}
	// A different principal may reuse the same request ID with different
	// content. It must neither conflict with the existing entries nor receive
	// either cached result.
	three := dialTest(t, endpoint, issueTicketForPrincipal(t, endpoint, "principal-C"))
	defer three.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, three, "principal-c-client")
	_ = readProtocol(t, three)
	writeProtocol(t, three, commandMessage("command-c", "same-request", "fake.principal", `{"value":2}`))
	if message := readProtocol(t, three); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("C accepted=%s", message.Kind())
	}
	if got := readCommandResult(t, three); string(got.Payload.Result) != `{"principal":"principal-C"}` {
		t.Fatalf("C independent result=%s", got.Payload.Result)
	}
	if executions.Load() != 3 {
		t.Fatalf("cross-principal conflict or dedupe, executions=%d", executions.Load())
	}
}

func TestDispatcherVolatileResultRefreshRetainsRequestIDTombstone(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	var executions atomic.Int64
	definition := commands.CommandDefinition{
		Name: "fake.volatile", SchemaVersion: 1, CachePolicy: commands.ResultCacheVolatile,
		Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
			count := executions.Add(1)
			return json.Marshal(map[string]int64{"execution": count})
		},
	}
	endpoint, _ := newDispatcherForTest(t, provider, definition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "volatile-client")
	_ = readProtocol(t, connection)
	writeProtocol(t, connection, commandMessage("volatile-first", "volatile-request", "fake.volatile", `{"value":1}`))
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("first accepted=%s", message.Kind())
	}
	first := readCommandResult(t, connection)
	if string(first.Payload.Result) != `{"execution":1}` {
		t.Fatalf("first volatile result=%s", first.Payload.Result)
	}

	// An exact retry regenerates the ephemeral result while preserving the
	// original request-id reservation.
	writeProtocol(t, connection, commandMessage("volatile-retry", "volatile-request", "fake.volatile", `{"value":1}`))
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("refresh accepted=%s", message.Kind())
	}
	second := readCommandResult(t, connection)
	if string(second.Payload.Result) != `{"execution":2}` || executions.Load() != 2 {
		t.Fatalf("refreshed volatile result=%s executions=%d", second.Payload.Result, executions.Load())
	}

	// The fingerprint tombstone still rejects a changed payload under the
	// same request ID.
	writeProtocol(t, connection, commandMessage("volatile-conflict", "volatile-request", "fake.volatile", `{"value":2}`))
	conflict := readCommandResult(t, connection)
	if conflict.Payload.Status != protocol.CommandStatusFailed || conflict.Payload.Error == nil || conflict.Payload.Error.Code != "idempotency_conflict" {
		t.Fatalf("volatile conflict=%#v", conflict)
	}
	if executions.Load() != 2 {
		t.Fatalf("conflicting volatile request executed: %d", executions.Load())
	}
}

func TestDispatcherCommandCacheCapacityRejectsNewKeysButRetainsExisting(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	var executions atomic.Int64
	definition := commands.CommandDefinition{Name: "fake.cached", SchemaVersion: 1, Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"ok":true}`), nil
	}}
	endpoint, _ := newDispatcherForTestWithOptions(t, provider, func(options *DispatcherOptions) { options.MaxCachedCommands = 1 }, definition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "cache-client")
	_ = readProtocol(t, connection)
	writeProtocol(t, connection, commandMessage("first", "cached-request", "fake.cached", `{}`))
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("accepted=%s", message.Kind())
	}
	if result := readCommandResult(t, connection); result.Payload.Status != protocol.CommandStatusSucceeded {
		t.Fatalf("first result=%#v", result)
	}
	writeProtocol(t, connection, commandMessage("second", "new-request", "fake.cached", `{}`))
	full := readCommandResult(t, connection)
	if full.Payload.Status != protocol.CommandStatusFailed || full.Payload.Error == nil || full.Payload.Error.Code != "command_cache_full" {
		t.Fatalf("cache full result=%#v", full)
	}
	writeProtocol(t, connection, commandMessage("retry", "cached-request", "fake.cached", `{}`))
	if result := readCommandResult(t, connection); result.Payload.Status != protocol.CommandStatusSucceeded {
		t.Fatalf("cached result=%#v", result)
	}
	if executions.Load() != 1 {
		t.Fatalf("execution count=%d, want 1", executions.Load())
	}
}

func TestDispatcherCommandCacheAdmissionIsAtomicAtConcurrentCapacity(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	var executions atomic.Int64
	started := make(chan struct{}, 2)
	definition := commands.CommandDefinition{Name: "fake.concurrent-cache", SchemaVersion: 1, Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
		executions.Add(1)
		started <- struct{}{}
		return json.RawMessage(`{}`), nil
	}}
	_, dispatcher := newDispatcherForTestWithOptions(t, provider, func(options *DispatcherOptions) { options.MaxCachedCommands = 2 }, definition)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	connection1 := &reservationConnection{info: ConnectionInfo{ConnectionID: "cache-concurrent-1", Principal: "p1"}}
	connection2 := &reservationConnection{info: ConnectionInfo{ConnectionID: "cache-concurrent-2", Principal: "p2"}}
	done := make(chan error, 2)
	go func() {
		done <- dispatcher.Handle(ctx1, connection1, commandMessage("one", "one", "fake.concurrent-cache", `{}`))
	}()
	go func() {
		done <- dispatcher.Handle(ctx2, connection2, commandMessage("two", "two", "fake.concurrent-cache", `{}`))
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-started
	<-started
	if dispatcher.CommandCacheCount() != 2 || executions.Load() != 2 {
		t.Fatalf("concurrent admission cache=%d executions=%d", dispatcher.CommandCacheCount(), executions.Load())
	}
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	connection3 := &reservationConnection{info: ConnectionInfo{ConnectionID: "cache-concurrent-3", Principal: "p3"}}
	if err := dispatcher.Handle(ctx3, connection3, commandMessage("three", "three", "fake.concurrent-cache", `{}`)); err != nil {
		t.Fatal(err)
	}
	messages := connection3.Messages()
	if len(messages) != 1 {
		t.Fatalf("full cache messages=%d", len(messages))
	}
	result, ok := messages[0].(protocol.CommandResultMessage)
	if !ok || result.Payload.Status != protocol.CommandStatusFailed || result.Payload.Error == nil || result.Payload.Error.Code != "command_cache_full" {
		t.Fatalf("full cache response=%#v", messages[0])
	}
	if executions.Load() != 2 {
		t.Fatalf("full cache executed side effect: %d", executions.Load())
	}
	cancel1()
	cancel2()
	cancel3()
	waitForDispatcherState(t, dispatcher, 0, 0)
}

func TestDispatcherCommandRejectionsUseFailedCommandResult(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	valid := commands.CommandDefinition{Name: "fake.valid", SchemaVersion: 1, Validate: func(arguments json.RawMessage) error {
		if string(arguments) != `{"ok":true}` {
			return context.Canceled
		}
		return nil
	}, Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}}
	panicDefinition := commands.CommandDefinition{Name: "fake.panic", SchemaVersion: 1, Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) { panic("must not escape") }}
	endpoint, _ := newDispatcherForTest(t, provider, valid, panicDefinition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "rejection-client")
	_ = readProtocol(t, connection)
	cases := []struct {
		name                string
		schema              int
		request, args, code string
	}{
		{"unknown", 1, "unknown", `{}`, "unknown_command"},
		{"schema", 2, "fake.valid", `{"ok":true}`, "unknown_command_schema"},
		{"validation", 1, "fake.valid", `{}`, "invalid_arguments"},
	}
	for _, test := range cases {
		writeProtocol(t, connection, protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: test.name}, Payload: protocol.CommandPayload{Name: test.request, SchemaVersion: test.schema, RequestID: "request-" + test.name, Arguments: json.RawMessage(test.args)}})
		result := readCommandResult(t, connection)
		if result.Payload.Status != protocol.CommandStatusFailed || result.Payload.Error == nil || result.Payload.Error.Code != test.code {
			t.Fatalf("%s result=%#v", test.name, result)
		}
	}
	writeProtocol(t, connection, commandMessage("accepted", "conflict-request", "fake.valid", `{"ok":true}`))
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("accepted=%s", message.Kind())
	}
	if result := readCommandResult(t, connection); result.Payload.Status != protocol.CommandStatusSucceeded {
		t.Fatalf("success=%#v", result)
	}
	writeProtocol(t, connection, commandMessage("conflict", "conflict-request", "fake.valid", `{"ok":false}`))
	conflict := readCommandResult(t, connection)
	if conflict.Payload.Status != protocol.CommandStatusFailed || conflict.Payload.Error == nil || conflict.Payload.Error.Code != "idempotency_conflict" {
		t.Fatalf("conflict=%#v", conflict)
	}
	writeProtocol(t, connection, commandMessage("panic", "panic-request", "fake.panic", `{}`))
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("panic accepted=%s", message.Kind())
	}
	panicResult := readCommandResult(t, connection)
	if panicResult.Payload.Status != protocol.CommandStatusFailed || panicResult.Payload.Error == nil || panicResult.Payload.Error.Code != "command_panic" {
		t.Fatalf("panic result=%#v", panicResult)
	}
}

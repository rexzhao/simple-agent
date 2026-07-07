package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const mailboxMCPProtocolVersion = "2025-06-18"

const (
	mailboxTaskQueued    = "queued"
	mailboxTaskRunning   = "running"
	mailboxTaskCompleted = "completed"
	mailboxTaskFailed    = "failed"
	mailboxTaskCancelled = "cancelled"
	mailboxTaskNotFound  = "not_found"
)

type mailboxQueue struct {
	mu     sync.Mutex
	tasks  map[string]*mailboxTask
	queue  []string
	nextID int64
	notify chan struct{}
}

type mailboxTask struct {
	ID         string
	Prompt     string
	Status     string
	Result     string
	Error      string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	done       chan struct{}
	cancel     context.CancelFunc
}

type mailboxTaskSnapshot struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"`
	Result   string `json:"result,omitempty"`
	Error    string `json:"error,omitempty"`
	TimedOut *bool  `json:"timed_out,omitempty"`
}

type mailboxTaskRead struct {
	task *mailboxTask
	err  error
}

type mailboxMCPServer struct {
	server *http.Server
	url    string
}

func newMailboxQueue() *mailboxQueue {
	return &mailboxQueue{
		tasks:  make(map[string]*mailboxTask),
		notify: make(chan struct{}),
	}
}

func (q *mailboxQueue) post(prompt string) (mailboxTaskSnapshot, error) {
	if strings.TrimSpace(prompt) == "" {
		return mailboxTaskSnapshot{}, errors.New("prompt is required")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.nextID++
	id := fmt.Sprintf("task_%06d", q.nextID)
	task := &mailboxTask{
		ID:        id,
		Prompt:    prompt,
		Status:    mailboxTaskQueued,
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
	}
	q.tasks[id] = task
	q.queue = append(q.queue, id)
	q.signalLocked()
	return task.snapshotLocked(), nil
}

func (q *mailboxQueue) get(id string) (mailboxTaskSnapshot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	task, ok := q.tasks[id]
	if !ok {
		return mailboxNotFoundSnapshot(id), false
	}
	return task.snapshotLocked(), true
}

func (q *mailboxQueue) wait(ctx context.Context, id string, timeout time.Duration) (mailboxTaskSnapshot, bool) {
	q.mu.Lock()
	task, ok := q.tasks[id]
	if !ok {
		q.mu.Unlock()
		return mailboxNotFoundSnapshot(id), false
	}
	if isTerminalMailboxStatus(task.Status) {
		snapshot := task.snapshotLocked()
		done := false
		snapshot.TimedOut = &done
		q.mu.Unlock()
		return snapshot, true
	}
	doneCh := task.done
	q.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	timedOut := false
	select {
	case <-doneCh:
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	task, ok = q.tasks[id]
	if !ok {
		return mailboxNotFoundSnapshot(id), false
	}
	snapshot := task.snapshotLocked()
	snapshot.TimedOut = &timedOut
	return snapshot, true
}

func (q *mailboxQueue) cancel(id string) (mailboxTaskSnapshot, bool) {
	q.mu.Lock()
	task, ok := q.tasks[id]
	if !ok {
		q.mu.Unlock()
		return mailboxNotFoundSnapshot(id), false
	}
	if isTerminalMailboxStatus(task.Status) {
		snapshot := task.snapshotLocked()
		q.mu.Unlock()
		return snapshot, true
	}

	cancel := task.cancel
	task.Status = mailboxTaskCancelled
	task.FinishedAt = time.Now()
	q.closeTaskLocked(task)
	q.signalLocked()
	snapshot := task.snapshotLocked()
	q.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return snapshot, true
}

func (q *mailboxQueue) dequeue(ctx context.Context) (*mailboxTask, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		for len(q.queue) > 0 {
			id := q.queue[0]
			q.queue = q.queue[1:]
			task, ok := q.tasks[id]
			if ok && task.Status == mailboxTaskQueued {
				return task, nil
			}
		}

		notify := q.notify
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			q.mu.Lock()
			return nil, ctx.Err()
		case <-notify:
			q.mu.Lock()
		}
	}
}

func (q *mailboxQueue) startTask(task *mailboxTask, cancel context.CancelFunc) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	current, ok := q.tasks[task.ID]
	if !ok || current.Status != mailboxTaskQueued {
		return false
	}
	current.Status = mailboxTaskRunning
	current.StartedAt = time.Now()
	current.cancel = cancel
	q.signalLocked()
	return true
}

func (q *mailboxQueue) completeTask(task *mailboxTask, result string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	current, ok := q.tasks[task.ID]
	if !ok || isTerminalMailboxStatus(current.Status) {
		return
	}
	current.Status = mailboxTaskCompleted
	current.Result = result
	current.cancel = nil
	current.FinishedAt = time.Now()
	q.closeTaskLocked(current)
	q.signalLocked()
}

func (q *mailboxQueue) failTask(task *mailboxTask, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	current, ok := q.tasks[task.ID]
	if !ok || isTerminalMailboxStatus(current.Status) {
		return
	}
	current.Status = mailboxTaskFailed
	if err != nil {
		current.Error = err.Error()
	}
	current.cancel = nil
	current.FinishedAt = time.Now()
	q.closeTaskLocked(current)
	q.signalLocked()
}

func (q *mailboxQueue) signalLocked() {
	close(q.notify)
	q.notify = make(chan struct{})
}

func (q *mailboxQueue) closeTaskLocked(task *mailboxTask) {
	select {
	case <-task.done:
	default:
		close(task.done)
	}
}

func (task *mailboxTask) snapshotLocked() mailboxTaskSnapshot {
	return mailboxTaskSnapshot{
		TaskID: task.ID,
		Status: task.Status,
		Result: task.Result,
		Error:  task.Error,
	}
}

func mailboxNotFoundSnapshot(id string) mailboxTaskSnapshot {
	return mailboxTaskSnapshot{
		TaskID: id,
		Status: mailboxTaskNotFound,
		Error:  "task not found",
	}
}

func isTerminalMailboxStatus(status string) bool {
	return status == mailboxTaskCompleted || status == mailboxTaskFailed || status == mailboxTaskCancelled
}

func startMailboxTaskRead(ctx context.Context, queue *mailboxQueue) <-chan mailboxTaskRead {
	ch := make(chan mailboxTaskRead, 1)
	go func() {
		task, err := queue.dequeue(ctx)
		ch <- mailboxTaskRead{task: task, err: err}
	}()
	return ch
}

func startMailboxMCPServer(ctx context.Context, addr string, queue *mailboxQueue) (*mailboxMCPServer, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("mailbox MCP address is required")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid mailbox MCP address: %w", err)
	}
	if !isLocalMailboxHost(host) {
		return nil, fmt.Errorf("mailbox MCP address must bind to localhost, 127.0.0.1, or ::1")
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	handler := &mailboxMCPHandler{queue: queue}
	mux.Handle("/mcp", handler)
	server := &http.Server{Handler: mux}
	mcp := &mailboxMCPServer{
		server: server,
		url:    "http://" + mailboxHTTPHost(listener.Addr()) + "/mcp",
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The CLI is foreground-owned; request handlers will surface bind errors before this point.
		}
	}()

	return mcp, nil
}

func (s *mailboxMCPServer) Close(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func mailboxHTTPHost(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return net.JoinHostPort(host, port)
}

func isLocalMailboxHost(host string) bool {
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

type mailboxMCPHandler struct {
	queue *mailboxQueue
}

func (h *mailboxMCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !mailboxOriginAllowed(r.Header.Get("Origin")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		writeMailboxMCPResponse(w, mailboxJSONRPCError(nil, -32700, "parse error"))
		return
	}

	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		h.serveBatch(r.Context(), w, raw)
		return
	}
	h.serveSingle(r.Context(), w, raw)
}

func (h *mailboxMCPHandler) serveBatch(ctx context.Context, w http.ResponseWriter, raw json.RawMessage) {
	var requests []mailboxJSONRPCRequest
	if err := json.Unmarshal(raw, &requests); err != nil || len(requests) == 0 {
		writeMailboxMCPResponse(w, mailboxJSONRPCError(nil, -32600, "invalid request"))
		return
	}

	responses := make([]mailboxJSONRPCResponse, 0, len(requests))
	for _, request := range requests {
		response, ok := h.handleRequest(ctx, request)
		if ok {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeMailboxMCPResponse(w, responses)
}

func (h *mailboxMCPHandler) serveSingle(ctx context.Context, w http.ResponseWriter, raw json.RawMessage) {
	var request mailboxJSONRPCRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeMailboxMCPResponse(w, mailboxJSONRPCError(nil, -32600, "invalid request"))
		return
	}
	response, ok := h.handleRequest(ctx, request)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeMailboxMCPResponse(w, response)
}

func (h *mailboxMCPHandler) handleRequest(ctx context.Context, request mailboxJSONRPCRequest) (mailboxJSONRPCResponse, bool) {
	if request.JSONRPC != "2.0" || request.Method == "" {
		return mailboxJSONRPCError(request.ID, -32600, "invalid request"), true
	}
	if len(request.ID) == 0 {
		return mailboxJSONRPCResponse{}, false
	}

	switch request.Method {
	case "initialize":
		return mailboxJSONRPCSuccess(request.ID, map[string]any{
			"protocolVersion": mailboxMCPProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "sai-mailbox",
				"version": Version,
			},
		}), true
	case "ping":
		return mailboxJSONRPCSuccess(request.ID, map[string]any{}), true
	case "tools/list":
		return mailboxJSONRPCSuccess(request.ID, map[string]any{
			"tools": mailboxMCPTools(),
		}), true
	case "tools/call":
		result, errResponse := h.handleToolCall(ctx, request.Params)
		if errResponse != nil {
			errResponse.JSONRPC = "2.0"
			errResponse.ID = request.ID
			return *errResponse, true
		}
		return mailboxJSONRPCSuccess(request.ID, result), true
	default:
		return mailboxJSONRPCError(request.ID, -32601, "method not found"), true
	}
}

func (h *mailboxMCPHandler) handleToolCall(ctx context.Context, params json.RawMessage) (mailboxMCPToolResult, *mailboxJSONRPCResponse) {
	var call mailboxMCPToolCallParams
	if len(params) == 0 {
		return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "invalid params"}}
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "invalid params"}}
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
	}

	switch call.Name {
	case "mailbox_post":
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "invalid arguments"}}
		}
		snapshot, err := h.queue.post(args.Prompt)
		if err != nil {
			return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: err.Error()}}
		}
		return mailboxMCPStructuredResult(snapshot, false), nil
	case "mailbox_get":
		id, errResponse := mailboxMCPTaskID(call.Arguments)
		if errResponse != nil {
			return mailboxMCPToolResult{}, errResponse
		}
		snapshot, ok := h.queue.get(id)
		return mailboxMCPStructuredResult(snapshot, !ok), nil
	case "mailbox_wait":
		var args struct {
			TaskID    string `json:"task_id"`
			TimeoutMS *int   `json:"timeout_ms,omitempty"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil || strings.TrimSpace(args.TaskID) == "" {
			return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "task_id is required"}}
		}
		timeoutMS := 30000
		if args.TimeoutMS != nil {
			timeoutMS = *args.TimeoutMS
		}
		if timeoutMS < 0 {
			return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "timeout_ms must be non-negative"}}
		}
		if timeoutMS > 300000 {
			timeoutMS = 300000
		}
		snapshot, ok := h.queue.wait(ctx, strings.TrimSpace(args.TaskID), time.Duration(timeoutMS)*time.Millisecond)
		return mailboxMCPStructuredResult(snapshot, !ok), nil
	case "mailbox_cancel":
		id, errResponse := mailboxMCPTaskID(call.Arguments)
		if errResponse != nil {
			return mailboxMCPToolResult{}, errResponse
		}
		snapshot, ok := h.queue.cancel(id)
		return mailboxMCPStructuredResult(snapshot, !ok), nil
	default:
		return mailboxMCPToolResult{}, &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "unknown tool"}}
	}
}

func mailboxMCPTaskID(arguments json.RawMessage) (string, *mailboxJSONRPCResponse) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil || strings.TrimSpace(args.TaskID) == "" {
		return "", &mailboxJSONRPCResponse{Error: &mailboxJSONRPCErrorObject{Code: -32602, Message: "task_id is required"}}
	}
	return strings.TrimSpace(args.TaskID), nil
}

func mailboxMCPStructuredResult(snapshot mailboxTaskSnapshot, isError bool) mailboxMCPToolResult {
	text, _ := json.Marshal(snapshot)
	return mailboxMCPToolResult{
		Content: []mailboxMCPContent{
			{Type: "text", Text: string(text)},
		},
		StructuredContent: snapshot,
		IsError:           isError,
	}
}

func mailboxMCPTools() []mailboxMCPTool {
	return []mailboxMCPTool{
		{
			Name:        "mailbox_post",
			Description: "Queue a prompt for the foreground sai CLI to run when it is idle.",
			InputSchema: mailboxObjectSchema(map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "Prompt to run.",
				},
			}, []string{"prompt"}),
		},
		{
			Name:        "mailbox_get",
			Description: "Return task status and final assistant output when available.",
			InputSchema: mailboxObjectSchema(map[string]any{
				"task_id": map[string]any{"type": "string"},
			}, []string{"task_id"}),
		},
		{
			Name:        "mailbox_wait",
			Description: "Wait for a task to finish up to timeout_ms without cancelling on timeout.",
			InputSchema: mailboxObjectSchema(map[string]any{
				"task_id": map[string]any{"type": "string"},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Timeout in milliseconds. Defaults to 30000 and is capped at 300000.",
					"minimum":     0,
					"maximum":     300000,
				},
			}, []string{"task_id"}),
		},
		{
			Name:        "mailbox_cancel",
			Description: "Cancel a queued task or interrupt the running mailbox turn.",
			InputSchema: mailboxObjectSchema(map[string]any{
				"task_id": map[string]any{"type": "string"},
			}, []string{"task_id"}),
		},
	}
}

func mailboxObjectSchema(properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func mailboxOriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLocalMailboxHost(parsed.Hostname())
}

func writeMailboxMCPResponse(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func mailboxJSONRPCSuccess(id json.RawMessage, result any) mailboxJSONRPCResponse {
	return mailboxJSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mailboxJSONRPCError(id json.RawMessage, code int, message string) mailboxJSONRPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return mailboxJSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mailboxJSONRPCErrorObject{Code: code, Message: message},
	}
}

type mailboxJSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mailboxJSONRPCResponse struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      json.RawMessage            `json:"id,omitempty"`
	Result  any                        `json:"result,omitempty"`
	Error   *mailboxJSONRPCErrorObject `json:"error,omitempty"`
}

type mailboxJSONRPCErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mailboxMCPToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type mailboxMCPToolResult struct {
	Content           []mailboxMCPContent `json:"content"`
	StructuredContent any                 `json:"structuredContent,omitempty"`
	IsError           bool                `json:"isError,omitempty"`
}

type mailboxMCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mailboxMCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func startMailboxForREPL(ctx context.Context, addr string, stderr io.Writer, displayCommand string) (*mailboxQueue, func(), error) {
	if strings.TrimSpace(addr) == "" {
		return nil, func() {}, nil
	}
	queue := newMailboxQueue()
	server, err := startMailboxMCPServer(ctx, addr, queue)
	if err != nil {
		return nil, func() {}, err
	}
	if _, err := fmt.Fprintf(stderr, "%s: mailbox MCP listening on %s\n", displayCommand, server.url); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Close(shutdownCtx)
		return nil, func() {}, err
	}
	stop := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Close(shutdownCtx)
	}
	return queue, stop, nil
}

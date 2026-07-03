package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	DefaultMaxJobs     = 64
	DefaultWaitTimeout = 30 * time.Second
	MaxWaitTimeout     = 30 * time.Second
	inboxBuffer        = 16
)

type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCanceled  JobStatus = "canceled"
)

type Message struct {
	Content string
}

type RunRequest struct {
	JobID       string
	AgentID     string
	ConfigPath  string
	Prompt      string
	DisplayName string
	JobName     string
	NextMessage func() (Message, bool)
}

type RunResult struct {
	Output string
}

type Runner interface {
	Run(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error)
}

type Option func(*Manager)

func WithMaxJobs(maxJobs int) Option {
	return func(m *Manager) {
		m.maxJobs = maxJobs
	}
}

func WithRootContext(ctx context.Context) Option {
	return func(m *Manager) {
		if ctx != nil {
			m.rootCtx = ctx
		}
	}
}

type Manager struct {
	mu            sync.Mutex
	wg            sync.WaitGroup
	configured    map[string]string
	runner        Runner
	rootCtx       context.Context
	maxJobs       int
	nextJobNumber int
	jobs          map[string]*job
	closed        bool
}

func NewManager(configured map[string]string, runner Runner, opts ...Option) (*Manager, error) {
	if runner == nil {
		return nil, fmt.Errorf("subagent runner is required")
	}

	m := &Manager{
		configured: make(map[string]string, len(configured)),
		runner:     runner,
		rootCtx:    context.Background(),
		maxJobs:    DefaultMaxJobs,
		jobs:       make(map[string]*job),
	}
	for id, configPath := range configured {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("subagent id must not be blank")
		}
		if strings.TrimSpace(configPath) == "" {
			return nil, fmt.Errorf("subagent %q config path must not be blank", id)
		}
		m.configured[id] = configPath
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	if m.rootCtx == nil {
		m.rootCtx = context.Background()
	}
	if m.maxJobs <= 0 {
		return nil, fmt.Errorf("max jobs must be greater than zero")
	}
	return m, nil
}

func (m *Manager) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	switch name {
	case ToolSubagentStart:
		return m.start(ctx, name, arguments)
	case ToolSubagentSend:
		return m.send(ctx, name, arguments)
	case ToolSubagentStatus:
		return m.status(name, arguments)
	case ToolSubagentWait:
		return m.wait(ctx, name, arguments)
	case ToolSubagentCancel:
		return m.cancel(name, arguments)
	default:
		return model.ToolResult{}, fmt.Errorf("unknown subagent tool %q", name)
	}
}

func (m *Manager) start(ctx context.Context, toolName string, arguments map[string]any) (model.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return errorResult(toolName, "subagent_start canceled: %v", err)
	}

	agentID, err := requiredStringArgument(arguments, "agent_id")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	prompt, err := optionalRawStringArgument(arguments, "prompt")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	displayName, err := optionalMetadataArgument(arguments, "display_name")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	jobName, err := optionalMetadataArgument(arguments, "job_name")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errorResult(toolName, "subagent manager is closed")
	}
	configPath, ok := m.configured[agentID]
	if !ok {
		m.mu.Unlock()
		return errorResult(toolName, "unknown subagent %q", agentID)
	}
	if len(m.jobs) >= m.maxJobs {
		m.mu.Unlock()
		return errorResult(toolName, "maximum subagent jobs reached (%d)", m.maxJobs)
	}

	m.nextJobNumber++
	jobID := fmt.Sprintf("subagent_job_%d", m.nextJobNumber)
	jobCtx, cancel := context.WithCancel(m.rootCtx)
	j := &job{
		id:          jobID,
		agentID:     agentID,
		displayName: displayName,
		jobName:     jobName,
		status:      StatusRunning,
		accepting:   true,
		inbox:       make(chan Message, inboxBuffer),
		done:        make(chan struct{}),
		cancel:      cancel,
	}
	m.jobs[jobID] = j
	snapshot := j.snapshotLocked()
	request := RunRequest{
		JobID:       jobID,
		AgentID:     agentID,
		ConfigPath:  configPath,
		Prompt:      prompt,
		DisplayName: displayName,
		JobName:     jobName,
		NextMessage: m.nextMessage(jobID),
	}
	inbox := j.inbox
	runner := m.runner
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		m.runJob(jobID, jobCtx, request, inbox, runner)
	}()
	return result(toolName, snapshot)
}

func (m *Manager) send(ctx context.Context, toolName string, arguments map[string]any) (model.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return errorResult(toolName, "subagent_send canceled: %v", err)
	}
	jobID, err := requiredStringArgument(arguments, "job_id")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	content, err := requiredRawStringArgument(arguments, "message")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}

	m.mu.Lock()
	j, ok := m.jobs[jobID]
	if !ok {
		m.mu.Unlock()
		return errorResult(toolName, "unknown subagent job %q", jobID)
	}
	if !canReceiveMessage(j.status) {
		status := j.status
		m.mu.Unlock()
		return errorResult(toolName, "subagent job %q is already %s", jobID, status)
	}
	if !j.accepting {
		m.mu.Unlock()
		return errorResult(toolName, "subagent job %q is no longer accepting messages", jobID)
	}

	select {
	case j.inbox <- Message{Content: content}:
		snapshot := j.snapshotLocked()
		snapshot.MessageQueued = true
		m.mu.Unlock()
		return result(toolName, snapshot)
	default:
		m.mu.Unlock()
		return errorResult(toolName, "subagent job %q inbox is full", jobID)
	}
}

func (m *Manager) status(toolName string, arguments map[string]any) (model.ToolResult, error) {
	jobID, err := requiredStringArgument(arguments, "job_id")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	snapshot, ok := m.snapshot(jobID)
	if !ok {
		return errorResult(toolName, "unknown subagent job %q", jobID)
	}
	return result(toolName, snapshot)
}

func (m *Manager) wait(ctx context.Context, toolName string, arguments map[string]any) (model.ToolResult, error) {
	jobID, err := requiredStringArgument(arguments, "job_id")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	timeoutMS, err := optionalIntegerArgument(arguments, "timeout_ms", int64(DefaultWaitTimeout/time.Millisecond))
	if err != nil {
		return errorResult(toolName, "%v", err)
	}
	if timeoutMS < 0 {
		return errorResult(toolName, "timeout_ms must be greater than or equal to 0")
	}
	if timeoutMS > int64(MaxWaitTimeout/time.Millisecond) {
		return errorResult(toolName, "timeout_ms must be less than or equal to %d", int64(MaxWaitTimeout/time.Millisecond))
	}

	m.mu.Lock()
	j, ok := m.jobs[jobID]
	if !ok {
		m.mu.Unlock()
		return errorResult(toolName, "unknown subagent job %q", jobID)
	}
	if isTerminalStatus(j.status) {
		snapshot := j.snapshotLocked()
		m.mu.Unlock()
		return result(toolName, snapshot)
	}
	done := j.done
	m.mu.Unlock()

	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-done:
		snapshot, _ := m.snapshot(jobID)
		return result(toolName, snapshot)
	case <-timer.C:
		snapshot, _ := m.snapshot(jobID)
		snapshot.TimedOut = true
		return result(toolName, snapshot)
	case <-ctx.Done():
		return errorResult(toolName, "subagent_wait canceled: %v", ctx.Err())
	}
}

func (m *Manager) cancel(toolName string, arguments map[string]any) (model.ToolResult, error) {
	jobID, err := requiredStringArgument(arguments, "job_id")
	if err != nil {
		return errorResult(toolName, "%v", err)
	}

	m.mu.Lock()
	j, ok := m.jobs[jobID]
	if !ok {
		m.mu.Unlock()
		return errorResult(toolName, "unknown subagent job %q", jobID)
	}
	if j.status == StatusCanceled {
		snapshot := j.snapshotLocked()
		m.mu.Unlock()
		return result(toolName, snapshot)
	}
	if isTerminalStatus(j.status) {
		status := j.status
		m.mu.Unlock()
		return errorResult(toolName, "subagent job %q is already %s", jobID, status)
	}

	j.status = StatusCanceled
	j.err = "canceled"
	j.accepting = false
	j.finish()
	cancel := j.cancel
	snapshot := j.snapshotLocked()
	m.mu.Unlock()

	cancel()
	return result(toolName, snapshot)
}

func (m *Manager) runJob(jobID string, ctx context.Context, request RunRequest, inbox <-chan Message, runner Runner) {
	runResult, err := runner.Run(ctx, request, inbox)

	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	if !ok {
		return
	}
	if j.status == StatusCanceled {
		j.accepting = false
		j.finish()
		return
	}
	j.accepting = false

	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			j.status = StatusCanceled
			j.err = "canceled"
		} else {
			j.status = StatusFailed
			j.err = err.Error()
		}
		j.finish()
		return
	}

	j.status = StatusCompleted
	j.output = runResult.Output
	j.finish()
}

func (m *Manager) nextMessage(jobID string) func() (Message, bool) {
	return func() (Message, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()

		j, ok := m.jobs[jobID]
		if !ok || !canReceiveMessage(j.status) || !j.accepting {
			return Message{}, false
		}

		select {
		case message := <-j.inbox:
			return message, true
		default:
			j.accepting = false
			return Message{}, false
		}
	}
}

func (m *Manager) Close() error {
	var cancels []context.CancelFunc
	m.mu.Lock()
	m.closed = true
	for _, j := range m.jobs {
		if isTerminalStatus(j.status) {
			j.accepting = false
			continue
		}
		j.status = StatusCanceled
		j.err = "canceled"
		j.accepting = false
		j.finish()
		if j.cancel != nil {
			cancels = append(cancels, j.cancel)
		}
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	m.wg.Wait()
	return nil
}

func (m *Manager) snapshot(jobID string) (JobSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	if !ok {
		return JobSnapshot{}, false
	}
	return j.snapshotLocked(), true
}

type job struct {
	id          string
	agentID     string
	configPath  string
	displayName string
	jobName     string
	status      JobStatus
	accepting   bool
	output      string
	err         string
	inbox       chan Message
	done        chan struct{}
	doneOnce    sync.Once
	cancel      context.CancelFunc
}

func (j *job) finish() {
	j.doneOnce.Do(func() {
		close(j.done)
	})
}

func (j *job) snapshotLocked() JobSnapshot {
	return JobSnapshot{
		OK:          true,
		JobID:       j.id,
		AgentID:     j.agentID,
		DisplayName: j.displayName,
		JobName:     j.jobName,
		Status:      j.status,
		Output:      j.output,
		Error:       j.err,
	}
}

type JobSnapshot struct {
	OK            bool      `json:"ok"`
	JobID         string    `json:"job_id"`
	AgentID       string    `json:"agent_id"`
	DisplayName   string    `json:"display_name,omitempty"`
	JobName       string    `json:"job_name,omitempty"`
	Status        JobStatus `json:"status"`
	Output        string    `json:"output,omitempty"`
	Error         string    `json:"error,omitempty"`
	TimedOut      bool      `json:"timed_out,omitempty"`
	MessageQueued bool      `json:"message_queued,omitempty"`
}

type errorPayload struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func result(toolName string, payload any) (model.ToolResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return model.ToolResult{}, err
	}
	return model.ToolResult{
		Name:    toolName,
		Content: string(data),
	}, nil
}

func errorResult(toolName string, format string, args ...any) (model.ToolResult, error) {
	data, err := json.Marshal(errorPayload{
		OK:    false,
		Error: fmt.Sprintf(format, args...),
	})
	if err != nil {
		return model.ToolResult{}, err
	}
	return model.ToolResult{
		Name:    toolName,
		Content: string(data),
		IsError: true,
	}, nil
}

func requiredStringArgument(arguments map[string]any, name string) (string, error) {
	text, err := requiredRawStringArgument(arguments, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func requiredRawStringArgument(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

func optionalRawStringArgument(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return text, nil
}

func optionalMetadataArgument(arguments map[string]any, name string) (string, error) {
	text, err := optionalRawStringArgument(arguments, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func optionalIntegerArgument(arguments map[string]any, name string, defaultValue int64) (int64, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}

	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(value.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		return parsed, nil
	case int:
		return int64(value), nil
	case int64:
		return value, nil
	case int32:
		return int64(value), nil
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		return int64(value), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", name)
	}
}

func canReceiveMessage(status JobStatus) bool {
	return status == StatusQueued || status == StatusRunning
}

func isTerminalStatus(status JobStatus) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCanceled
}

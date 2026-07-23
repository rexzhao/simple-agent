//go:build windows

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	windowsProcessTerminate = 0x0001
	windowsProcessSetQuota  = 0x0100

	windowsJobObjectExtendedLimitInformationClass = 9
	windowsJobObjectLimitKillOnJobClose           = 0x00002000
)

var (
	windowsShellKernel32                     = syscall.NewLazyDLL("kernel32.dll")
	windowsShellCreateJobObjectProc          = windowsShellKernel32.NewProc("CreateJobObjectW")
	windowsShellSetInformationJobObjectProc  = windowsShellKernel32.NewProc("SetInformationJobObject")
	windowsShellAssignProcessToJobObjectProc = windowsShellKernel32.NewProc("AssignProcessToJobObject")
	windowsShellOpenProcessProc              = windowsShellKernel32.NewProc("OpenProcess")
	windowsShellCloseHandleProc              = windowsShellKernel32.NewProc("CloseHandle")
)

// windowsJobObjectExtendedLimitInformation is the Windows SDK
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION layout. Kill-on-close ensures that a
// shell process and every descendant in its job terminate together when the
// command is cancelled or finishes.
type windowsJobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type windowsIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type windowsJobObjectExtendedLimitInformation struct {
	BasicLimitInformation windowsJobObjectBasicLimitInformation
	IOInfo                windowsIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func newShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
}

type windowsShellCommandController struct {
	cmd *exec.Cmd

	mu        sync.Mutex
	job       syscall.Handle
	cancelled bool
	closed    bool
}

func newShellCommandController(cmd *exec.Cmd) shellCommandController {
	controller := &windowsShellCommandController{cmd: cmd}
	cmd.WaitDelay = shellCancelWaitDelay
	cmd.Cancel = controller.cancel
	return controller
}

func (c *windowsShellCommandController) Run(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// Assigning the root process to a kill-on-close job makes ordinary
	// completion, context cancellation, and tool teardown all own the same
	// descendant process tree. If a host has nested-job restrictions, cancel
	// still falls back to a bounded taskkill /T /F invocation.
	_ = c.attachJob(cmd.Process.Pid)
	err := cmd.Wait()
	c.Close()
	return err
}

func (c *windowsShellCommandController) cancel() error {
	c.mu.Lock()
	c.cancelled = true
	job := c.job
	c.mu.Unlock()
	if job != 0 {
		if err := c.closeJob(); err == nil || errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	return killWindowsProcessTree(c.processID())
}

func (c *windowsShellCommandController) Close() {
	_ = c.closeJob()
}

func (c *windowsShellCommandController) processID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *windowsShellCommandController) attachJob(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	job, err := createWindowsKillOnCloseJob()
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = closeWindowsHandle(job)
		}
	}()

	process, err := openWindowsProcessForJob(pid)
	if err != nil {
		return err
	}
	defer closeWindowsHandle(process)
	if err := assignWindowsProcessToJob(job, process); err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed || c.cancelled {
		c.mu.Unlock()
		return os.ErrProcessDone
	}
	c.job = job
	c.mu.Unlock()
	cleanup = false
	return nil
}

func (c *windowsShellCommandController) closeJob() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return os.ErrProcessDone
	}
	c.closed = true
	job := c.job
	c.job = 0
	c.mu.Unlock()
	if job == 0 {
		return os.ErrProcessDone
	}
	return closeWindowsHandle(job)
}

func createWindowsKillOnCloseJob() (syscall.Handle, error) {
	result, _, callErr := windowsShellCreateJobObjectProc.Call(0, 0)
	if result == 0 {
		return 0, windowsCallError("create shell job object", callErr)
	}
	job := syscall.Handle(result)
	info := windowsJobObjectExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = windowsJobObjectLimitKillOnJobClose
	result, _, callErr = windowsShellSetInformationJobObjectProc.Call(
		uintptr(job),
		uintptr(windowsJobObjectExtendedLimitInformationClass),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if result == 0 {
		_ = closeWindowsHandle(job)
		return 0, windowsCallError("set shell job limits", callErr)
	}
	return job, nil
}

func openWindowsProcessForJob(pid int) (syscall.Handle, error) {
	result, _, callErr := windowsShellOpenProcessProc.Call(
		uintptr(windowsProcessTerminate|windowsProcessSetQuota),
		0,
		uintptr(uint32(pid)),
	)
	if result == 0 {
		return 0, windowsCallError("open shell process for job", callErr)
	}
	return syscall.Handle(result), nil
}

func assignWindowsProcessToJob(job, process syscall.Handle) error {
	result, _, callErr := windowsShellAssignProcessToJobObjectProc.Call(uintptr(job), uintptr(process))
	if result == 0 {
		return windowsCallError("assign shell process to job", callErr)
	}
	return nil
}

func closeWindowsHandle(handle syscall.Handle) error {
	if handle == 0 {
		return os.ErrProcessDone
	}
	result, _, callErr := windowsShellCloseHandleProc.Call(uintptr(handle))
	if result == 0 {
		return windowsCallError("close shell job handle", callErr)
	}
	return nil
}

func windowsCallError(operation string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return fmt.Errorf("%s: %w", operation, syscall.EINVAL)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func killWindowsProcessTree(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellCancelWaitDelay)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	cmd.WaitDelay = shellCancelWaitDelay
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("taskkill process tree pid %d: %w: %s", pid, err, strings.TrimSpace(string(output)))
}

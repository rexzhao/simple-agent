package webapp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInstanceLockAcquireReleaseRoundTrip(t *testing.T) {
	root := t.TempDir()
	first, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("first acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("first acquireInstanceLock = false, want true")
	}
	// 同一文件的两个独立 fd 必须互斥。
	second, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("second acquireInstanceLock error = %v", err)
	}
	if acquired {
		t.Fatalf("second acquireInstanceLock = true while first holds lock, want false")
	}
	if second != nil {
		t.Fatalf("second acquireInstanceLock returned non-nil lock on failure")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("first.Release() error = %v", err)
	}
	third, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("third acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("third acquireInstanceLock = false after release, want true")
	}
	if err := third.Release(); err != nil {
		t.Fatalf("third.Release() error = %v", err)
	}
}

func TestInstanceRegistryReadWriteRoundTrip(t *testing.T) {
	root := t.TempDir()
	lock, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("acquireInstanceLock = false, want true")
	}
	defer lock.Release()

	want := instanceRegistry{
		PID:       os.Getpid(),
		BaseURL:   "http://127.0.0.1:12345/",
		Token:     "abc123",
		StartedAt: "2026-01-01T00:00:00Z",
		Ready:     true,
	}
	if err := lock.writeRegistry(want); err != nil {
		t.Fatalf("writeRegistry error = %v", err)
	}
	got := readRegistry(root)
	if got != want {
		t.Fatalf("readRegistry = %#v, want %#v", got, want)
	}
}

func TestReadRegistryToleratesPartialFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, instanceFileName)
	data := make([]byte, instanceRegistryOffset)
	data = append(data, []byte(`{"pid": 123, "base_url": "http://127.0.0.1:9`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	got := readRegistry(root)
	if got.Ready || got.BaseURL != "" {
		t.Fatalf("readRegistry(partial) = %#v, want empty registry", got)
	}
}

func TestAcquireInstanceReusesExistingInstance(t *testing.T) {
	// 起一个假的已有实例服务。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	root := t.TempDir()
	first, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("acquireInstanceLock = false, want true")
	}
	defer first.Release()
	if err := first.writeRegistry(instanceRegistry{
		PID:       999,
		BaseURL:   server.URL + "/",
		Token:     "tok123",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Ready:     true,
	}); err != nil {
		t.Fatalf("writeRegistry error = %v", err)
	}

	// 拦截 openBrowser，记录是否被调用。
	var mu sync.Mutex
	opened := false
	openBrowser = func(url string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = true
		if !strings.HasPrefix(url, server.URL+"/#token=tok123") {
			t.Errorf("openBrowser url = %q, want prefix %q", url, server.URL+"/#token=tok123")
		}
		return nil
	}
	defer func() { openBrowser = openBrowserImpl }()

	var stdout, stderr bytes.Buffer
	lock, exitCode, mustExit, err := acquireInstance(root, instanceAcquireOptions{
		stdout: &stdout,
		stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("acquireInstance error = %v", err)
	}
	if lock != nil {
		t.Fatalf("acquireInstance returned non-nil lock on reuse path")
	}
	if !mustExit || exitCode != 0 {
		t.Fatalf("acquireInstance mustExit/exitCode = %v/%d, want true/0", mustExit, exitCode)
	}
	mu.Lock()
	openedCopy := opened
	mu.Unlock()
	if !openedCopy {
		t.Fatalf("openBrowser was not called on reuse path")
	}
}

func TestAcquireInstanceNoOpenReusesWithoutOpening(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	root := t.TempDir()
	first, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("acquireInstanceLock = false, want true")
	}
	defer first.Release()
	if err := first.writeRegistry(instanceRegistry{
		PID:       999,
		BaseURL:   server.URL + "/",
		Token:     "tok123",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Ready:     true,
	}); err != nil {
		t.Fatalf("writeRegistry error = %v", err)
	}

	var mu sync.Mutex
	opened := false
	openBrowser = func(url string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = true
		return nil
	}
	defer func() { openBrowser = openBrowserImpl }()

	var stdout, stderr bytes.Buffer
	lock, exitCode, mustExit, err := acquireInstance(root, instanceAcquireOptions{
		noOpen: true,
		stdout: &stdout,
		stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("acquireInstance error = %v", err)
	}
	if lock != nil || !mustExit || exitCode != 0 {
		t.Fatalf("acquireInstance lock/mustExit/exitCode = %v/%v/%d, want nil/true/0", lock, mustExit, exitCode)
	}
	mu.Lock()
	openedCopy := opened
	mu.Unlock()
	if openedCopy {
		t.Fatalf("openBrowser was called with noOpen=true")
	}
	if !strings.Contains(stdout.String(), "SAI_WEB_URL") {
		t.Fatalf("stdout = %q, want SAI_WEB_URL", stdout.String())
	}
}

func TestAcquireInstanceTakesOverWhenHolderDies(t *testing.T) {
	root := t.TempDir()
	first, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("acquireInstanceLock = false, want true")
	}
	// 模拟持有者崩溃：写入一个过期 ready 注册表但不释放锁对象，直接 close。
	if err := first.writeRegistry(instanceRegistry{
		PID:       999,
		BaseURL:   "http://127.0.0.1:1/",
		Token:     "dead",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Ready:     true,
	}); err != nil {
		t.Fatalf("writeRegistry error = %v", err)
	}
	// 持有者退出：close 释放锁（等价于进程退出）。
	if err := first.file.Close(); err != nil {
		t.Fatalf("close instance file error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	lock, exitCode, mustExit, err := acquireInstance(root, instanceAcquireOptions{
		stdout: &stdout,
		stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("acquireInstance error = %v", err)
	}
	if mustExit {
		t.Fatalf("acquireInstance mustExit = true, want takeover (false)")
	}
	if lock == nil {
		t.Fatalf("acquireInstance returned nil lock on takeover path")
	}
	if exitCode != 0 {
		t.Fatalf("acquireInstance exitCode = %d, want 0", exitCode)
	}
	defer lock.Release()
	// 接管后注册表应被清空为未 ready。
	got := readRegistry(root)
	if got.Ready {
		t.Fatalf("readRegistry after takeover = %#v, want not ready", got)
	}
}

func TestAcquireInstanceTimesOutWhenHolderNeverReady(t *testing.T) {
	root := t.TempDir()
	first, acquired, err := acquireInstanceLock(root)
	if err != nil {
		t.Fatalf("acquireInstanceLock error = %v", err)
	}
	if !acquired {
		t.Fatalf("acquireInstanceLock = false, want true")
	}
	defer first.Release()
	// 持有者一直未 ready（注册表从未写入）。

	// 缩短超时，避免测试等待 5s。
	oldTimeout := instanceReusePollTimeout
	instanceReusePollTimeout = 300 * time.Millisecond
	defer func() { instanceReusePollTimeout = oldTimeout }()

	var stdout, stderr bytes.Buffer
	lock, exitCode, mustExit, err := acquireInstance(root, instanceAcquireOptions{
		stdout: &stdout,
		stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("acquireInstance error = %v", err)
	}
	if lock != nil {
		t.Fatalf("acquireInstance returned non-nil lock on timeout path")
	}
	if !mustExit || exitCode != 0 {
		t.Fatalf("acquireInstance mustExit/exitCode = %v/%d, want true/0", mustExit, exitCode)
	}
	if !strings.Contains(stderr.String(), "another instance") {
		t.Fatalf("stderr = %q, want timeout message", stderr.String())
	}
}

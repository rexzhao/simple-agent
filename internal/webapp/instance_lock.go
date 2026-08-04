package webapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const instanceFileName = "instance.json"

const instanceReusePollInterval = 100 * time.Millisecond

// instanceReusePollTimeout 是等待已有实例 ready 的超时，测试可缩短。
var instanceReusePollTimeout = 5 * time.Second

// instanceRegistry 是运行实例的注册表，同时作为单实例锁文件。
// 锁由 OS 在进程退出/崩溃时自动释放；注册表内容据此就地更新，永不 rename。
type instanceRegistry struct {
	PID       int    `json:"pid"`
	BaseURL   string `json:"base_url"`
	Token     string `json:"token"`
	StartedAt string `json:"started_at"`
	Ready     bool   `json:"ready"`
}

// instanceLock 持有 server root 的跨进程排他锁。
type instanceLock struct {
	file     *os.File
	path     string
	released bool
}

// instanceAcquireOptions 是 acquireInstance 的选项。
type instanceAcquireOptions struct {
	noOpen      bool
	stdout      io.Writer
	stderr      io.Writer
	healthCheck func(baseURL string) bool
}

// acquireInstanceLock 尝试非阻塞获取 server root 的单实例排他锁。
// 返回 (nil, false, nil) 表示锁被其他实例持有。
func acquireInstanceLock(root string) (*instanceLock, bool, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, false, fmt.Errorf("create server root %q: %w", root, err)
	}
	path := filepath.Join(root, instanceFileName)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open instance file %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("restrict instance file %q: %w", path, err)
	}
	locked, err := tryLockInstanceFile(file)
	if err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("lock instance file %q: %w", path, err)
	}
	if !locked {
		_ = file.Close()
		return nil, false, nil
	}
	return &instanceLock{file: file, path: path}, true, nil
}

// Release 释放实例锁。锁文件本身不删除，避免删除后第二个进程新建文件
// 成功加锁的竞态。
func (l *instanceLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return errors.Join(unlockInstanceFile(l.file), l.file.Close())
}

// instanceRegistryOffset 是注册表 JSON 在锁文件中的起始偏移。
// Windows 的 LockFileEx 是排他字节区间锁，其他进程读被锁区域会失败；
// 因此锁文件以固定 1 字节哨兵（offset 0）作为锁区间，JSON 从 offset 1 开始写，
// 读方跳过 offset 0 读取，从而保持单文件互斥的同时可读。
const instanceRegistryOffset = 1

// writeRegistry 就地写入注册表，必须在持有锁时调用。不能使用 rename：
// rename 会改变锁所在的文件对象，导致后续进程对路径新建文件后能成功加锁，
// 破坏单实例约束。
func (l *instanceLock) writeRegistry(registry instanceRegistry) error {
	if l == nil || l.released {
		return fmt.Errorf("instance lock is not held")
	}
	data, err := json.Marshal(registry)
	if err != nil {
		return fmt.Errorf("encode instance registry: %w", err)
	}
	if err := l.file.Truncate(instanceRegistryOffset); err != nil {
		return fmt.Errorf("truncate instance file %q: %w", l.path, err)
	}
	if _, err := l.file.Seek(instanceRegistryOffset, 0); err != nil {
		return fmt.Errorf("seek instance file %q: %w", l.path, err)
	}
	if _, err := l.file.Write(data); err != nil {
		return fmt.Errorf("write instance file %q: %w", l.path, err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync instance file %q: %w", l.path, err)
	}
	return nil
}

// readRegistry 读取锁文件里的注册表。文件可能处于写入中途（截断后、写完前），
// 解析失败一律视为未 ready，由调用方轮询重试。
func readRegistry(root string) instanceRegistry {
	path := filepath.Join(root, instanceFileName)
	file, err := os.Open(path)
	if err != nil {
		return instanceRegistry{}
	}
	defer file.Close()
	if _, err := file.Seek(instanceRegistryOffset, 0); err != nil {
		return instanceRegistry{}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return instanceRegistry{}
	}
	var registry instanceRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return instanceRegistry{}
	}
	return registry
}

// probeInstanceHealth 探测 baseURL 是否可服务。只关心能否建立连接；
// 不需要鉴权，因为 "/" 是静态入口。
func probeInstanceHealth(baseURL string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(baseURL)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// acquireInstance 保证 server root 单实例，并在已有实例时复用其浏览器。
// 返回：
//
//	lock     - 非 nil 表示本进程成为实例，调用方必须 defer Release。
//	exitCode - mustExit 时的退出码。
//	mustExit - true 表示本进程应退出（复用了已有实例，或等待超时）。
//	err      - 加锁环节的错误（视为启动失败）。
func acquireInstance(root string, options instanceAcquireOptions) (lock *instanceLock, exitCode int, mustExit bool, err error) {
	lock, acquired, err := acquireInstanceLock(root)
	if err != nil {
		return nil, 0, false, err
	}
	if acquired {
		// 立即清空注册表，避免复用读到上一个实例的过期 ready 记录。
		if err := lock.writeRegistry(instanceRegistry{Ready: false}); err != nil {
			return nil, 0, false, fmt.Errorf("clear instance registry: %w", err)
		}
		return lock, 0, false, nil
	}

	healthCheck := options.healthCheck
	if healthCheck == nil {
		healthCheck = probeInstanceHealth
	}
	deadline := time.Now().Add(instanceReusePollTimeout)
	for {
		// 持有者可能已退出：每次迭代优先重新拿锁接管，而不是信任过期注册表。
		lock, acquired, err := acquireInstanceLock(root)
		if err != nil {
			return nil, 0, false, err
		}
		if acquired {
			if err := lock.writeRegistry(instanceRegistry{Ready: false}); err != nil {
				return nil, 0, false, fmt.Errorf("clear instance registry: %w", err)
			}
			return lock, 0, false, nil
		}
		registry := readRegistry(root)
		if registry.Ready && registry.BaseURL != "" && registry.Token != "" && healthCheck(registry.BaseURL) {
			url := registry.BaseURL + "#token=" + registry.Token
			if options.noOpen {
				fmt.Fprintf(options.stdout, "SAI_WEB_URL\t%s\n", url)
				fmt.Fprintln(options.stderr, "sai: already running; web url: "+url)
			} else {
				if err := openBrowser(url); err != nil {
					fmt.Fprintf(options.stderr, "sai: open browser: %v\n", err)
				} else {
					fmt.Fprintln(options.stderr, "sai: already running; browser opened")
				}
			}
			return nil, 0, true, nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(options.stderr, "sai: another instance is already starting for this server root; try again shortly")
			return nil, 0, true, nil
		}
		time.Sleep(instanceReusePollInterval)
	}
}

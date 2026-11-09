//go:build windows

package uistate

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// renameWithRetry 在 Windows 上带重试地改名，并在重试前尝试清掉目标文件。
//
// Windows 的 `MoveFile`（os.Rename 底层）在目标**已存在**时默认会失败，
// 而且 `%APPDATA%\Roaming` 是漫游目录：同步服务、搜索索引器、杀毒软件都会
// 短暂持有里面的文件，于是保存偏好时会看到
// `rename ...ui-state-*.tmp ...ui-state.json: Access is denied`。
//
// 处理顺序（每一步都比上一步更强势）：
//  1. 直接 rename（目标不存在时最理想）；
//  2. 重试若干次（占用通常是几十毫秒级的）；
//  3. 先删目标再 rename（目标只读或残留句柄时）；
//  4. 退化为“直接覆盖写目标文件”——原子性差一点，但**偏好必须能存下去**，
//     否则用户每拖一次都会看到报错。
func renameWithRetry(from, to string) error {
	var lastErr error
	// 阶段 1+2：直接 rename，失败后短暂重试
	for attempt := 0; attempt < 5; attempt++ {
		err := os.Rename(from, to)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isPermissionErr(err) {
			// 不是占用/权限问题，重试没意义
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !isPermissionErr(lastErr) {
		return lastErr
	}

	// 阶段 3：删掉目标再 rename（目标只读时 os.Remove 也会失败，一并重试）
	for attempt := 0; attempt < 3; attempt++ {
		if err := os.Remove(to); err == nil || errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := os.Rename(from, to); err == nil {
		return nil
	} else {
		lastErr = err
	}

	// 阶段 4：放弃原子性，直接覆盖写（rename 走不通时最后的办法）
	data, readErr := os.ReadFile(from)
	if readErr != nil {
		return lastErr
	}
	if writeErr := os.WriteFile(to, data, 0o644); writeErr != nil {
		return lastErr
	}
	return nil
}

// isPermissionErr 判断错误是不是“文件被占用/无权访问”（值得重试的那些）。
func isPermissionErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return true
	}
	// ERROR_SHARING_VIOLATION / ERROR_LOCK_VIOLATION：文件正被别的进程打开
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}

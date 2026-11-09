// Package uistate 保存**本机界面偏好**（窗口/布局这类纯客户端状态）。
//
// 与 store 包的区别（这是关键设计决定）：
//   - `store` 存的是**会话内容**，属于执行端；远程模式下只有执行端有它。
//   - 这里的偏好是**每台客户端自己的**：正文列多宽、侧栏是否收起，取决于
//     你面前的屏幕和输入设备，不该跟着执行端走。所以它存在**系统默认的应用
//     配置目录**，而不是 `<data_dir>`：
//     Windows `%APPDATA%\simple-agent`、Linux `$XDG_CONFIG_HOME/simple-agent`、
//     macOS `~/Library/Application Support/simple-agent`。
//
// 因此展示端（`--connect`）也能有自己的偏好，且不需要数据目录锁、不会与
// 执行端抢同一个文件。这是 dsh 用 localStorage 达到的同一效果。
//
// 容错策略：**文件损坏绝不能影响启动**。读失败/字段非法一律回默认值，
// 下一次写入自然把它修好。
package uistate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Version 是文件格式版本（以后加字段时用它判断要不要迁移）。
const Version = 1

// FileName 是偏好文件名。
const FileName = "ui-state.json"

// State 是本机界面偏好。
type State struct {
	Version int `json:"version"`
	// ChatContentWidth 是正文内容列宽度（px）。0 表示没设置过，按窗口自适应。
	//
	// 这个值由“会话宽度把手”拖动产生，与 dsh 的
	// `dsh.conversation.contentWidth` 对应。
	ChatContentWidth int `json:"chat_content_width,omitempty"`
}

// Default 返回默认偏好（全部“没设置”）。
func Default() State { return State{Version: Version} }

// Path 返回偏低文件的完整路径（系统默认应用配置目录下的 simple-agent/）。
//
// 取不到配置目录时返回空字符串：调用方据此退化为“不持久化”，
// 而不是把文件写到奇怪的位置（比如当前目录）。
func Path() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "simple-agent", FileName)
}

// Load 读取偏好文件。
//
// 三种“没有偏好”的情况都返回默认值且 **err == nil**：文件不存在、
// 内容损坏、版本不认识。理由：偏好丢了是小事，为它拦住用户启动是大事。
// 真正需要用户知道的错误（比如磁盘读不了）也仍然回默认值，但把原因带出去。
func Load(path string) (State, error) {
	state := Default()
	if path == "" {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, fmt.Errorf("读取界面偏好失败：%w", err)
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		// 损坏：回默认值，下次保存会覆盖掉坏文件
		return Default(), fmt.Errorf("界面偏好文件已损坏，按默认值处理：%w", err)
	}
	// 只认已知版本；将来的版本由那时的代码迁移
	if loaded.Version != Version {
		return Default(), nil
	}
	return normalize(loaded), nil
}

// normalize 把非法值收敛到合法范围（外部改坏文件也不能让界面画出负数宽度）。
func normalize(state State) State {
	state.Version = Version
	if state.ChatContentWidth < 0 {
		state.ChatContentWidth = 0
	}
	if state.ChatContentWidth > 0 && state.ChatContentWidth < minContentWidth {
		state.ChatContentWidth = minContentWidth
	}
	return state
}

// minContentWidth 是正文列的最小宽度（与 uiapp 的 CONTENT_MIN 对齐；
// 这里再夹一次，防止手改文件把界面挤成一条线）。
const minContentWidth = 640

// Save 原子写入偏好文件（tmp + rename，与 store/blobs 的做法一致）。
//
// 空 path 直接返回：调用方在“拿不到配置目录”时不该因此报错。
func Save(path string, state State) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建偏好目录失败：%w", err)
	}
	state.Version = Version
	data, err := json.MarshalIndent(normalize(state), "", "  ")
	if err != nil {
		return fmt.Errorf("编码界面偏好失败：%w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "ui-state-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入界面偏好失败：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步界面偏好失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭界面偏好临时文件失败：%w", err)
	}
	// rename 在 Windows 上可能被漫游目录的同步/索引服务挡住，
	// renameWithRetry 会重试并给出更强的回退（见 fs_windows.go）。
	if err := renameWithRetry(tmpName, path); err != nil {
		return fmt.Errorf("保存界面偏好失败：%w", err)
	}
	return nil
}

package uiapp

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// 记住上次连过的 relay：`/connect` 不带参数时直接复用。
//
// 存成独立的小文件（`<data_dir>/relay.json`），不写进 simple-agent.yaml：
// 配置文件是用户手写的，程序回写会破坏注释与排版。这个文件只保存
// "上次成功连接用的参数"，属于运行状态。

// savedRelay 是落盘的 relay 连接参数。
type savedRelay struct {
	Addr   string `json:"addr"`
	Token  string `json:"token,omitempty"`
	// Fingerprint 是证书指纹（启用 TLS 时非空）。
	Fingerprint string `json:"fingerprint,omitempty"`
}

// relayStatePath 返回保存路径（空表示没有数据目录，不保存）。
func (a *App) relayStatePath() string {
	dir := a.dataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "relay.json")
}

// loadSavedRelay 读上次保存的 relay 参数（没有则返回零值）。
func (a *App) loadSavedRelay() savedRelay {
	path := a.relayStatePath()
	if path == "" {
		return savedRelay{}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return savedRelay{}
	}
	var saved savedRelay
	if err := json.Unmarshal(content, &saved); err != nil {
		return savedRelay{}
	}
	return saved
}

// saveRelay 记住这次成功连接的参数。
//
// 只在**连接成功后**调用：连不上的地址不值得记住，否则下次 `/connect`
// 会拿一个错地址反复失败，用户还得先想怎么清掉。
func (a *App) saveRelay(params savedRelay) {
	path := a.relayStatePath()
	if path == "" {
		return
	}
	content, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return
	}
	// 权限 0600：里面有 token
	_ = os.WriteFile(path, content, 0o600)
}

// clearSavedRelay 忘掉保存的 relay（`/disconnect --forget` 用）。
func (a *App) clearSavedRelay() {
	if path := a.relayStatePath(); path != "" {
		_ = os.Remove(path)
	}
}

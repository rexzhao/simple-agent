package main

import (
	"simple-agent/internal/host"
	"simple-agent/internal/relay"
	"simple-agent/internal/uiapp"
)

// relayExecutors 是界面的"执行端列表/切换"实现（只经 relay 时存在）。
type relayExecutors struct {
	addr    string
	remote  *host.Remote
	current *pinnedHost
	// token 是 relay 的固定口令（relay 没配 token 时为空）。
	token string
}

func (r *relayExecutors) Executors() ([]uiapp.ExecutorRef, error) {
	current := r.Current()
	list, err := relay.ListExecutorsToken(r.addr, r.token)
	refs := make([]uiapp.ExecutorRef, 0, len(list)+1)
	seen := map[string]bool{}
	for _, executor := range list {
		refs = append(refs, uiapp.ExecutorRef{
			HostID: executor.HostID, Name: executor.Name, Online: true,
		})
		seen[executor.HostID] = true
	}
	// 当前连着的执行端暂时不在线：仍然列出来（显示为离线），否则列表会突然少了它
	if current != "" && !seen[current] {
		refs = append(refs, uiapp.ExecutorRef{HostID: current, Name: current, Online: false})
	}
	return refs, err
}

func (r *relayExecutors) Current() string {
	if r.current == nil {
		return ""
	}
	r.current.Lock()
	defer r.current.Unlock()
	return r.current.hostID
}

func (r *relayExecutors) Switch(hostID string) error {
	if r.current == nil {
		return nil
	}
	if r.remote == nil {
		return nil
	}
	r.current.Lock()
	r.current.hostID = hostID
	r.current.Unlock()
	// 顺序很关键：先关旧连接（确认不会再产生新包）→ 加代际并清空旧状态 →
	// 再重连。否则旧机器已经排进界面队列的 stream/catalog 会落到新机器上。
	r.remote.DropConn()
	r.remote.ResetForNewHost()
	r.remote.RetryNow()
	return nil
}

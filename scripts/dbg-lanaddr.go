//go:build ignore

// 诊断：relay 下发的执行端信息里，局域网地址带不带指纹。
package main

import (
	"fmt"

	"simple-agent/internal/relay"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/wire"
)

func main() {
	cfg := tlsx.Config{Fingerprint: "2c6ba220221da9d84be0574ec57a594ab86fc0b95df4e1372b8282de7d6acc23"}
	previous := wire.DialFunc()
	wire.SetDialFunc(cfg.Dial)
	defer wire.SetDialFunc(previous)

	list, err := relay.ListExecutorsToken("129.211.171.158:7900", "relaytest2024")
	if err != nil {
		fmt.Println("查询失败:", err)
		return
	}
	for _, e := range list {
		fmt.Printf("执行端 %s: %d 个局域网地址\n", e.HostID, len(e.LANAddrs))
		for _, a := range e.LANAddrs {
			fmt.Printf("  %s  指纹=%q  可拨=%v\n", a.String(), a.TLSFingerprint, a.Dialable())
		}
	}
}

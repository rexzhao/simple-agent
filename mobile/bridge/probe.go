// Package bridge 是 Android 客户端与 Go 内核之间的唯一接口。
//
// 详见 bridge.go。
package bridge

// Ping 是一个探针：用来验证 gomobile 绑定链路（AAR 生成、加载、调用）。
func Ping() string { return "pong" }

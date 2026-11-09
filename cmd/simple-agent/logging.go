package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// setupLogging 把日志同时写到 stderr 和 <data_dir>/simple-agent[-role].log。
//
// 发布版是双击启动的 GUI，没有控制台；不落文件的话，"为什么中断了"这类
// 关键日志用户根本看不到。
//
// 按角色分文件：同一个数据目录下可能同时有执行端与展示端（relay 模式很常见），
// 两个进程写同一个文件会互相穿插，也分不清是谁的日志。
func setupLogging(dataDir, role string) (func(), io.Writer) {
	if dataDir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return func() {}, nil
	}
	name := "simple-agent.log"
	switch role {
	case "viewer":
		name = "simple-agent-viewer.log"
	case "relay":
		name = "simple-agent-relay.log"
	}
	path := filepath.Join(dataDir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return func() {}, nil
	}
	log.SetOutput(io.MultiWriter(os.Stderr, file))
	log.Printf("---- 启动 simple-agent，日志写入 %s ----", path)
	return func() { _ = file.Close() }, file
}

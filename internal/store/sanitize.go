package store

import (
	"reflect"
	"strings"
	"unicode/utf8"
)

// replacement 是非法 UTF-8 字节的替换字符（与 JSON 规范、Go 的 ToValidUTF8 一致）。
const replacement = "\uFFFD"

// SanitizeUTF8 把不是合法 UTF-8 的字节换成 U+FFFD。
//
// 为什么需要它：工具输出**不保证是 UTF-8**。中文 Windows 上 `read_file` 读一个
// GBK 编码的源文件、`search` 命中 GBK 内容、或者命令吐二进制，都会带出非法字节；
// `encoding/json/v2` 遇到这种字符串会**直接报错**，而写日志的调用点用的是
// `MustEvent`（panic）。用户实测崩溃过一次：`tool.results` 的 `output` 里有非法
// UTF-8，整个界面线程 panic。
//
// 这里做的是"兜底"：真正的解码在 tools 层（`decodeConsole` 按 OEM 代码页转、
// 文件读取按 UTF-8 校验），到这一步还剩下的就是无法识别的字节，换成替换字符比
// 丢掉整条日志好。
func SanitizeUTF8(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	return strings.ToValidUTF8(text, replacement)
}

// sanitizeValue 深拷贝 v，把其中所有字符串/字节切片修成合法 UTF-8。
//
// 只在 `NewEvent` 编码失败之后的兜底路径上调用（正常路径没有额外开销）。
// 用反射而不是先 marshal 再修：json/v2 在 marshal 阶段就会拒绝非法 UTF-8，
// 拿不到可以修补的中间结果。
func sanitizeValue(v any) any {
	return sanitizeReflect(reflect.ValueOf(v)).Interface()
}

func sanitizeReflect(rv reflect.Value) reflect.Value {
	if !rv.IsValid() {
		return rv
	}
	switch rv.Kind() {
	case reflect.String:
		return reflect.ValueOf(SanitizeUTF8(rv.String())).Convert(rv.Type())
	case reflect.Slice:
		if rv.IsNil() {
			return rv
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			// []byte：编码时会被当成 base64 或按 JSON 规则处理，非法字节照样会
			// 触发错误（json/v2 对 []byte 走 base64，不受影响；保险起见也修一遍）
			return reflect.ValueOf([]byte(SanitizeUTF8(string(rv.Bytes())))).Convert(rv.Type())
		}
		out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out.Index(i).Set(sanitizeReflect(rv.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(rv.Type()).Elem()
		for i := 0; i < rv.Len(); i++ {
			out.Index(i).Set(sanitizeReflect(rv.Index(i)))
		}
		return out
	case reflect.Map:
		if rv.IsNil() {
			return rv
		}
		out := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out.SetMapIndex(sanitizeReflect(iter.Key()), sanitizeReflect(iter.Value()))
		}
		return out
	case reflect.Pointer:
		if rv.IsNil() {
			return rv
		}
		out := reflect.New(rv.Type().Elem())
		out.Elem().Set(sanitizeReflect(rv.Elem()))
		return out
	case reflect.Interface:
		if rv.IsNil() {
			return rv
		}
		inner := sanitizeReflect(rv.Elem())
		out := reflect.New(rv.Type()).Elem()
		out.Set(inner)
		return out
	case reflect.Struct:
		out := reflect.New(rv.Type()).Elem()
		for i := 0; i < rv.NumField(); i++ {
			if !out.Field(i).CanSet() {
				return rv // 有不可导出的字段：放弃修补，保持原值
			}
			out.Field(i).Set(sanitizeReflect(rv.Field(i)))
		}
		return out
	default:
		return rv
	}
}

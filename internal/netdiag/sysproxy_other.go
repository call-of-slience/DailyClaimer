//go:build !windows

package netdiag

// SystemProxy 非 Windows 平台不做系统代理解析（macOS/Linux 走环境变量即可）。
func SystemProxy() (enabled bool, server string) { return false, "" }

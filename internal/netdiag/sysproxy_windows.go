//go:build windows

package netdiag

import (
	"strings"
	"syscall"
	"unsafe"
)

// Windows 系统代理（Internet 选项 → 局域网设置）存在注册表里。
// 这是「代理软件关了、但系统代理开关还开着」的根源：浏览器与 Chrome 会照着
// 这个已死的端口发请求，而 Go 只认环境变量，所以出现「浏览器连不上、程序能连」
// 这种看似矛盾的现象。
const (
	hkeyCurrentUser  = 0x80000001
	keyRead          = 0x20019
	internetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

var (
	modAdvapi32     = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKey  = modAdvapi32.NewProc("RegOpenKeyExW")
	procRegQueryVal = modAdvapi32.NewProc("RegQueryValueExW")
	procRegCloseKey = modAdvapi32.NewProc("RegCloseKey")
)

func regString(h syscall.Handle, name string) string {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	var typ, size uint32
	if r, _, _ := procRegQueryVal.Call(uintptr(h), uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&size))); r != 0 || size == 0 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if r, _, _ := procRegQueryVal.Call(uintptr(h), uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func regDWORD(h syscall.Handle, name string) (uint32, bool) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, false
	}
	var typ, size uint32
	var val uint32
	size = 4
	if r, _, _ := procRegQueryVal.Call(uintptr(h), uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&val)), uintptr(unsafe.Pointer(&size))); r != 0 {
		return 0, false
	}
	return val, true
}

// SystemProxy 返回当前系统代理开关与服务器地址（形如 127.0.0.1:2080）。
func SystemProxy() (enabled bool, server string) {
	p, err := syscall.UTF16PtrFromString(internetSettings)
	if err != nil {
		return false, ""
	}
	var h syscall.Handle
	if r, _, _ := procRegOpenKey.Call(uintptr(hkeyCurrentUser), uintptr(unsafe.Pointer(p)), 0,
		keyRead, uintptr(unsafe.Pointer(&h))); r != 0 {
		return false, ""
	}
	defer procRegCloseKey.Call(uintptr(h))

	en, _ := regDWORD(h, "ProxyEnable")
	server = strings.TrimSpace(regString(h, "ProxyServer"))
	if server == "" && strings.TrimSpace(regString(h, "AutoConfigURL")) != "" {
		// PAC 模式下拿不到具体地址，交给系统自己处理
		return en != 0, ""
	}
	return en != 0, server
}

//go:build windows

package sysproxy

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKeyPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func Set(enabled bool, listenAddr string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if enabled {
		proxyServer := normalizeListen(listenAddr)
		if err := k.SetStringValue("ProxyServer", proxyServer); err != nil {
			return err
		}
		if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
			return err
		}
		if err := k.SetStringValue("ProxyOverride", defaultBypassList()); err != nil {
			return err
		}
	} else {
		if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
			return err
		}
	}

	notifyWinInet()
	return nil
}

func normalizeListen(listenAddr string) string {
	l := strings.TrimSpace(listenAddr)
	if l == "" {
		return "127.0.0.1:30000"
	}
	if strings.Contains(l, ":") {
		return l
	}
	return "127.0.0.1:" + l
}

func defaultBypassList() string {
	return "localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*;<local>"
}

func notifyWinInet() {
	wininet := syscall.NewLazyDLL("wininet.dll")
	proc := wininet.NewProc("InternetSetOptionW")
	const (
		internetOptionSettingsChanged = 39
		internetOptionRefresh         = 37
	)
	_, _, _ = proc.Call(0, uintptr(internetOptionSettingsChanged), 0, 0)
	_, _, _ = proc.Call(0, uintptr(internetOptionRefresh), 0, 0)
}

func IsEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil {
		return false, nil
	}
	return v != 0, nil
}

func CurrentServer() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return "", nil
	}
	return v, nil
}

func DebugString() string {
	en, _ := IsEnabled()
	ps, _ := CurrentServer()
	return fmt.Sprintf("enabled=%v proxyServer=%q", en, ps)
}

// keep unsafe import used (some Windows syscall builds require it)
var _ = unsafe.Pointer(nil)

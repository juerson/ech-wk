//go:build darwin

package sysproxy

import (
	"fmt"
	"os/exec"
	"strings"
)

// Network service name usually "Wi-Fi" or "Ethernet".
func getPrimaryService() string {
	return "Wi-Fi"
}

func Set(enabled bool, listenAddr string) error {
	service := getPrimaryService()
	if !enabled {
		_ = exec.Command("networksetup", "-setwebproxystate", service, "off").Run()
		_ = exec.Command("networksetup", "-setsecurewebproxystate", service, "off").Run()
		_ = exec.Command("networksetup", "-setsocksfirewallproxystate", service, "off").Run()
		return nil
	}

	host, port := splitHostPort(listenAddr)
	if host == "" {
		host = "127.0.0.1"
	}

	if err := exec.Command("networksetup", "-setwebproxy", service, host, port).Run(); err != nil {
		return fmt.Errorf("failed to set web proxy: %v", err)
	}
	if err := exec.Command("networksetup", "-setsecurewebproxy", service, host, port).Run(); err != nil {
		return fmt.Errorf("failed to set secure web proxy: %v", err)
	}
	return nil
}

func splitHostPort(addr string) (string, string) {
	parts := strings.Split(addr, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "127.0.0.1", "8080"
}

func IsEnabled() (bool, error) {
	return false, nil
}

func CurrentServer() (string, error) {
	return "", nil
}

//go:build linux

package sysproxy

import (
	"fmt"
	"os/exec"
	"strings"
)

func Set(enabled bool, listenAddr string) error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return fmt.Errorf("gsettings not found")
	}

	if !enabled {
		exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "none").Run()
		return nil
	}

	host, port := splitHostPort(listenAddr)

	exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "manual").Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.http", "host", host).Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.http", "port", port).Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.https", "host", host).Run()
	exec.Command("gsettings", "set", "org.gnome.system.proxy.https", "port", port).Run()

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

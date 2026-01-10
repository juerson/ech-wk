//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"text/template"
)

const desktopFileTemplate = `[Desktop Entry]
Type=Application
Name=ECH Workers Client
Exec={{.ExePath}}
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
`

func getDesktopFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "autostart")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "ech-workers-client.desktop"), nil
}

func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	path, err := getDesktopFilePath()
	if err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	t := template.Must(template.New("desktop").Parse(desktopFileTemplate))
	return t.Execute(f, map[string]string{"ExePath": exe})
}

func Disable() error {
	path, err := getDesktopFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func IsEnabled() (bool, error) {
	path, err := getDesktopFilePath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

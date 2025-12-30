package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type LastState struct {
	WasRunning         bool `json:"was_running"`
	SystemProxyEnabled bool `json:"system_proxy_enabled"`
	AutoStartChecked   bool `json:"auto_start_checked"`
	PreferredMode      int  `json:"preferred_mode"` // 0=自动检测, 1=内嵌模式, 2=外部模式
}

type Server struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Server      string `json:"server"`
	Listen      string `json:"listen"`
	Token       string `json:"token"`
	IP          string `json:"ip"`
	DNS         string `json:"dns"`
	ECH         string `json:"ech"`
	RoutingMode string `json:"routing_mode"`
}

type FileModel struct {
	Servers         []Server  `json:"servers"`
	CurrentServerID string    `json:"current_server_id"`
	LastState       LastState `json:"last_state"`
}

type Manager struct {
	ConfigDir  string
	ConfigFile string
	Model      FileModel
}

func NewManager() (*Manager, error) {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return nil, errors.New("APPDATA is not set")
	}
	configDir := filepath.Join(appdata, "ECHWorkersClient")
	configFile := filepath.Join(configDir, "config.json")
	m := &Manager{ConfigDir: configDir, ConfigFile: configFile}
	m.Model.LastState = LastState{}
	return m, nil
}

func (m *Manager) Load() error {
	if err := os.MkdirAll(m.ConfigDir, 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(m.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			m.Model = FileModel{Servers: []Server{}, CurrentServerID: "", LastState: LastState{}}
			return nil
		}
		return err
	}
	var model FileModel
	if err := json.Unmarshal(b, &model); err != nil {
		return err
	}
	m.Model = model
	return nil
}

func (m *Manager) Save() error {
	if err := os.MkdirAll(m.ConfigDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.Model, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.ConfigFile, b, 0o644)
}

func (m *Manager) GetCurrentServer() (Server, bool) {
	if len(m.Model.Servers) == 0 {
		return Server{}, false
	}
	if m.Model.CurrentServerID != "" {
		for _, s := range m.Model.Servers {
			if s.ID == m.Model.CurrentServerID {
				return s, true
			}
		}
	}
	return m.Model.Servers[0], true
}

func (m *Manager) SetCurrentServer(id string) {
	m.Model.CurrentServerID = id
}

func (m *Manager) UpsertServer(s Server) {
	for i := range m.Model.Servers {
		if m.Model.Servers[i].ID == s.ID {
			m.Model.Servers[i] = s
			return
		}
	}
	m.Model.Servers = append(m.Model.Servers, s)
}

func (m *Manager) DeleteServer(id string) {
	out := make([]Server, 0, len(m.Model.Servers))
	for _, s := range m.Model.Servers {
		if s.ID != id {
			out = append(out, s)
		}
	}
	m.Model.Servers = out
	if m.Model.CurrentServerID == id {
		if len(m.Model.Servers) > 0 {
			m.Model.CurrentServerID = m.Model.Servers[0].ID
		} else {
			m.Model.CurrentServerID = ""
		}
	}
}

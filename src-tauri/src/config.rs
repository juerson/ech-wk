use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use dirs;
use anyhow::{Result, anyhow};
use log::info;
use std::fs;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LastState {
    pub was_running: bool,
    pub system_proxy_enabled: bool,
    pub auto_start_checked: bool,
    pub preferred_mode: i32, // 0=自动检测, 1=内嵌模式(代码没有写), 2=外部模式
}

impl Default for LastState {
    fn default() -> Self {
        Self {
            was_running: false,
            system_proxy_enabled: false,
            auto_start_checked: false,
            preferred_mode: 0,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Server {
    pub id: String,
    pub name: String,
    pub server: String,
    pub listen: String,
    pub token: String,
    pub ip: String,
    pub dns: String,
    pub ech: String,
    pub routing_mode: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FileModel {
    pub servers: Vec<Server>,
    pub current_server_id: String,
    pub last_state: LastState,
}

impl Default for FileModel {
    fn default() -> Self {
        Self {
            servers: Vec::new(),
            current_server_id: String::new(),
            last_state: LastState::default(),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProxyConfig {
    pub listen_addr: String,
    pub server_addr: String,
    pub server_ip: String,
    pub token: String,
    pub dns_server: String,
    pub ech_domain: String,
    pub routing_mode: String,
}

impl Default for ProxyConfig {
    fn default() -> Self {
        Self {
            listen_addr: "127.0.0.1:30000".to_string(),
            server_addr: "example.workers.dev:443".to_string(),
            server_ip: String::new(),  // 优选地址可选，默认为空
            token: String::new(),      // 身份令牌可选，默认为空
            dns_server: "dns.alidns.com/dns-query".to_string(),
            ech_domain: "cloudflare-ech.com".to_string(),
            routing_mode: "global".to_string(),
        }
    }
}

#[derive(Debug)]
pub struct Config {
    config_dir: PathBuf,
    config_file: PathBuf,
    model: FileModel,
}

impl Default for Config {
    fn default() -> Self {
        let config_dir = get_config_dir().unwrap_or_else(|_| PathBuf::from("."));
        let config_file = config_dir.join("config.json");
        
        Config {
            config_dir,
            config_file,
            model: FileModel::default(),
        }
    }
}

impl Config {
    pub fn load() -> Result<Self> {
        let config_dir = get_config_dir()?;
        let config_file = config_dir.join("config.json");

        let mut config = Config {
            config_dir,
            config_file,
            model: FileModel::default(),
        };

        if config.config_file.exists() {
            let content = fs::read_to_string(&config.config_file)
                .map_err(|e| anyhow!("Failed to read config file: {}", e))?;
            
            config.model = serde_json::from_str(&content)
                .map_err(|e| anyhow!("Failed to parse config file: {}", e))?;
            
            info!("Configuration loaded from {:?}", config.config_file);
        } else {
            info!("Config file not found, using defaults");
            config.ensure_default_server()?;
            config.save()?;
        }

        // 确保至少有一个服务器配置
        config.ensure_default_server()?;

        Ok(config)
    }

    fn ensure_default_server(&mut self) -> Result<()> {
        if self.model.servers.is_empty() {
            let default_server = Server {
                id: generate_server_id(),
                name: "默认服务器".to_string(),
                server: "example.workers.dev:443".to_string(),
                listen: "127.0.0.1:30000".to_string(),
                token: String::new(),
                ip: String::new(),
                dns: "dns.alidns.com/dns-query".to_string(),
                ech: "cloudflare-ech.com".to_string(),
                routing_mode: "global".to_string(),
            };
            
            self.model.servers.push(default_server);
            self.model.current_server_id = self.model.servers[0].id.clone();
            
            info!("Created default server configuration");
        }
        Ok(())
    }

    pub fn save(&self) -> Result<()> {
        fs::create_dir_all(&self.config_dir)
            .map_err(|e| anyhow!("Failed to create config directory: {}", e))?;

        let content = serde_json::to_string_pretty(&self.model)
            .map_err(|e| anyhow!("Failed to serialize config: {}", e))?;

        fs::write(&self.config_file, content)
            .map_err(|e| anyhow!("Failed to write config file: {}", e))?;

        info!("Configuration saved to {:?}", self.config_file);
        Ok(())
    }

    pub fn get_proxy_config(&self) -> ProxyConfig {
        if let Some(server) = self.get_current_server() {
            ProxyConfig {
                listen_addr: server.listen.clone(),
                server_addr: server.server.clone(),
                server_ip: server.ip.clone(),
                token: server.token.clone(),
                dns_server: server.dns.clone(),
                ech_domain: server.ech.clone(),
                routing_mode: server.routing_mode.clone(),
            }
        } else {
            ProxyConfig::default()
        }
    }

    pub fn set_proxy_config(&mut self, config: ProxyConfig) {
        if let Some(mut server) = self.get_current_server() {
            server.listen = config.listen_addr;
            server.server = config.server_addr;
            server.ip = config.server_ip;
            server.token = config.token;
            server.dns = config.dns_server;
            server.ech = config.ech_domain;
            server.routing_mode = config.routing_mode;
            
            self.upsert_server(server);
        }
    }

    pub fn get_servers(&self) -> Vec<Server> {
        self.model.servers.clone()
    }

    pub fn get_current_server(&self) -> Option<Server> {
        if self.model.servers.is_empty() {
            return None;
        }

        if !self.model.current_server_id.is_empty() {
            if let Some(server) = self.model.servers.iter()
                .find(|s| s.id == self.model.current_server_id) {
                return Some(server.clone());
            }
        }

        // Fallback to first server
        self.model.servers.first().cloned()
    }

    pub fn set_current_server(&mut self, id: String) {
        self.model.current_server_id = id;
    }

    pub fn upsert_server(&mut self, server: Server) {
        if let Some(existing) = self.model.servers.iter_mut()
            .find(|s| s.id == server.id) {
            *existing = server;
        } else {
            self.model.servers.push(server);
        }
    }

    pub fn delete_server(&mut self, id: &str) {
        self.model.servers.retain(|s| s.id != id);
        
        if self.model.current_server_id == id {
            self.model.current_server_id = self.model.servers
                .first()
                .map(|s| s.id.clone())
                .unwrap_or_default();
        }
    }

    pub fn get_last_state(&self) -> LastState {
        self.model.last_state.clone()
    }

    pub fn set_last_state(&mut self, state: LastState) {
        self.model.last_state = state;
    }
}

fn get_config_dir() -> Result<PathBuf> {
    let app_data = dirs::config_dir()
        .ok_or_else(|| anyhow!("Could not find config directory"))?;
    
    let config_dir = app_data.join("ECHWorkersClient");
    Ok(config_dir)
}

#[allow(dead_code)]
pub fn generate_server_id() -> String {
    use rand::Rng;
    let mut rng = rand::thread_rng();
    format!("server_{:x}", rng.gen::<u32>())
}

// #[cfg(test)]
// mod tests {
//     use super::*;

//     #[test]
//     fn test_generate_server_id() {
//         let id1 = generate_server_id();
//         let id2 = generate_server_id();
//         assert_ne!(id1, id2);
//         assert!(id1.starts_with("server_"));
//     }

//     #[test]
//     fn test_config_default() {
//         let config = ProxyConfig::default();
//         assert_eq!(config.listen_addr, "127.0.0.1:8080");
//         assert_eq!(config.routing_mode, "global");
//         assert_eq!(config.dns_server, "dns.alidns.com/dns-query");
//     }
// }

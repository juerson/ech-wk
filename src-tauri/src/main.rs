// Prevents additional console window on Windows in release, DO NOT REMOVE!!
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use ech_workers_client_lib::AppState;
use ech_workers_client_lib::external_proxy::{ExternalProxyServer, get_output, clear_output};
use ech_workers_client_lib::config::{Config, ProxyConfig, Server, LastState};
use ech_workers_client_lib::tray;
use tauri::{Manager, State};

#[derive(Clone, serde::Serialize)]
pub struct ProxyStatusInfo {
    is_running: bool,
    is_managed_running: bool,
    is_external_running: bool,
    system_proxy_enabled: bool,
}

#[tauri::command]
async fn start_proxy(
    state: State<'_, AppState>,
    config: ProxyConfig,
) -> Result<String, String> {
    if check_external_process_running().await {
        return Ok("External proxy server already running externally".to_string());
    }
    {
        let guard = state
            .proxy_server
            .lock()
            .map_err(|_| "Proxy mutex poisoned")?;

        if guard.is_some() {
            return Err("External proxy server is already running".to_string());
        }
    }

    let mut server = ExternalProxyServer::new(config)
        .map_err(|e| format!("Failed to create proxy server: {}", e))?;

    server
        .start()
        .await
        .map_err(|e| format!("Failed to start proxy server: {}", e))?;

    {
        let mut guard = state
            .proxy_server
            .lock()
            .map_err(|_| "Proxy mutex poisoned")?;

        *guard = Some(server);
    }

    {
        let mut config_guard = state.config.lock().await;
        let mut last_state = config_guard.get_last_state();
        last_state.was_running = true;
        config_guard.set_last_state(last_state);
        let _ = config_guard.save();
    }

    Ok("External proxy server started successfully".to_string())
}

#[tauri::command]
async fn stop_proxy(state: State<'_, AppState>) -> Result<String, String> {
    let mut server = {
        let mut guard = state
            .proxy_server
            .lock()
            .map_err(|_| "Proxy mutex poisoned")?;

        guard.take()
    };

    if let Some(server) = server.as_mut() {
        server
            .stop()
            .await
            .map_err(|e| format!("Failed to stop proxy server: {}", e))?;

        // 停止代理时同时禁用系统代理
        if let Err(e) = ech_workers_client_lib::sysproxy::disable_system_proxy() {
            log::warn!("Failed to disable system proxy: {}", e);
        } else {
            // 保存状态：系统代理已禁用
            let mut config_guard = state.config.lock().await;
            let mut last_state = config_guard.get_last_state();
            last_state.system_proxy_enabled = false;
            last_state.was_running = false;
            config_guard.set_last_state(last_state);

            if let Err(e) = config_guard.save() {
                log::error!("Failed to save config after disabling system proxy: {}", e);
            }
            drop(config_guard);
        }

        Ok("External proxy server stopped successfully".to_string())
    } else {
        Err("External proxy server is not running".to_string())
    }
}

#[tauri::command]
async fn get_proxy_status(state: State<'_, AppState>) -> Result<ProxyStatusInfo, String> {
    let is_managed_running = {
        let guard = state
            .proxy_server
            .lock()
            .map_err(|_| "Proxy mutex poisoned")?;
        guard.is_some()
    };

    // 检查系统中是否有 ech-workers.exe 进程在运行
    let is_external_running = check_external_process_running().await;

    // 获取系统代理状态
    let system_proxy_enabled = ech_workers_client_lib::sysproxy::is_system_proxy_enabled()
        .unwrap_or(false);

    Ok(ProxyStatusInfo {
        is_running: is_managed_running || is_external_running,
        is_managed_running,
        is_external_running,
        system_proxy_enabled,
    })
}

/// 检查系统中是否有 ech-workers.exe 进程在运行
async fn check_external_process_running() -> bool {
    #[cfg(windows)]
    {
        use std::process::Command;
        use std::os::windows::process::CommandExt;

        // 使用 tasklist 命令查找 ech-workers.exe 进程
        let output = Command::new("tasklist")
            .args(&["/FI", "IMAGENAME eq ech-workers.exe", "/FO", "CSV"])
            .creation_flags(0x08000000) // CREATE_NO_WINDOW
            .output();

        match output {
            Ok(result) => {
                let output_str = String::from_utf8_lossy(&result.stdout);
                output_str.contains("ech-workers.exe")
            }
            Err(_) => false,
        }
    }

    #[cfg(not(windows))]
    {
        // 在非Windows系统上，可以使用 ps 命令
        use std::process::Command;

        let output = Command::new("ps")
            .args(&["aux"])
            .output();

        match output {
            Ok(result) => {
                let output_str = String::from_utf8_lossy(&result.stdout);
                output_str.contains("ech-workers")
            }
            Err(_) => false,
        }
    }
}

#[tauri::command]
async fn get_config(state: State<'_, AppState>) -> Result<ProxyConfig, String> {
    let config_state = state.config.lock().await;
    Ok(config_state.get_proxy_config())
}

#[tauri::command]
async fn save_config(
    state: State<'_, AppState>,
    config: ProxyConfig,
) -> Result<(), String> {
    let mut config_state = state.config.lock().await;
    config_state.set_proxy_config(config);
    match config_state.save() {
        Ok(_) => Ok(()),
        Err(e) => Err(format!("Failed to save config: {}", e)),
    }
}

#[tauri::command]
async fn get_servers(state: State<'_, AppState>) -> Result<Vec<Server>, String> {
    let config_state = state.config.lock().await;
    Ok(config_state.get_servers())
}

#[tauri::command]
async fn get_current_server(state: State<'_, AppState>) -> Result<Option<Server>, String> {
    let config_state = state.config.lock().await;
    Ok(config_state.get_current_server())
}

#[tauri::command]
async fn set_current_server(
    state: State<'_, AppState>,
    server_id: String,
) -> Result<(), String> {
    let mut config_state = state.config.lock().await;
    config_state.set_current_server(server_id);
    match config_state.save() {
        Ok(_) => Ok(()),
        Err(e) => Err(format!("Failed to save config: {}", e)),
    }
}

#[tauri::command]
async fn upsert_server(
    state: State<'_, AppState>,
    server: Server,
) -> Result<(), String> {
    let mut config_state = state.config.lock().await;
    config_state.upsert_server(server);
    match config_state.save() {
        Ok(_) => Ok(()),
        Err(e) => Err(format!("Failed to save config: {}", e)),
    }
}

#[tauri::command]
async fn delete_server(
    state: State<'_, AppState>,
    server_id: String,
) -> Result<(), String> {
    let mut config_state = state.config.lock().await;
    config_state.delete_server(&server_id);
    match config_state.save() {
        Ok(_) => Ok(()),
        Err(e) => Err(format!("Failed to save config: {}", e)),
    }
}

#[tauri::command]
async fn set_system_proxy(enabled: bool, state: State<'_, AppState>) -> Result<(), String> {
    if enabled {
        // 从已保存的配置中读取监听地址
        let config_state = state.config.lock().await;
        let proxy_config = config_state.get_proxy_config();
        let listen_addr = proxy_config.listen_addr;
        drop(config_state);

        // 解析地址和端口
        let parts: Vec<&str> = listen_addr.split(':').collect();
        if parts.len() != 2 {
            return Err("Invalid listen address format".to_string());
        }

        let host = parts[0];
        let port = parts[1];

        ech_workers_client_lib::sysproxy::set_system_proxy(host, port)
            .map_err(|e| format!("Failed to set system proxy: {}", e))?;

        // 保存状态：系统代理已启用
        let mut config_guard = state.config.lock().await;
        let mut last_state = config_guard.get_last_state();
        last_state.system_proxy_enabled = true;
        config_guard.set_last_state(last_state);

        if let Err(e) = config_guard.save() {
            log::error!("Failed to save config after enabling system proxy: {}", e);
        }
        drop(config_guard);
    } else {
        ech_workers_client_lib::sysproxy::disable_system_proxy()
            .map_err(|e| format!("Failed to disable system proxy: {}", e))?;

        // 保存状态：系统代理已禁用
        let mut config_guard = state.config.lock().await;
        let mut last_state = config_guard.get_last_state();
        last_state.system_proxy_enabled = false;
        config_guard.set_last_state(last_state);

        if let Err(e) = config_guard.save() {
            log::error!("Failed to save config after disabling system proxy: {}", e);
        }
        drop(config_guard);
    }
    Ok(())
}

#[tauri::command]
async fn set_autostart(enabled: bool) -> Result<(), String> {
    if enabled {
        ech_workers_client_lib::autostart::enable_autostart()
            .map_err(|e| format!("Failed to enable autostart: {}", e))?;
    } else {
        ech_workers_client_lib::autostart::disable_autostart()
            .map_err(|e| format!("Failed to disable autostart: {}", e))?;
    }
    Ok(())
}

#[tauri::command]
async fn get_proxy_output() -> Result<Vec<String>, String> {
    Ok(get_output())
}

#[tauri::command]
async fn get_last_state(state: State<'_, AppState>) -> Result<LastState, String> {
    let config_guard = state.config.lock().await;
    Ok(config_guard.get_last_state())
}

#[tauri::command]
async fn clear_proxy_output() -> Result<(), String> {
    clear_output();
    Ok(())
}

#[tauri::command]
async fn get_autostart_status() -> Result<bool, String> {
    match ech_workers_client_lib::autostart::is_autostart_enabled() {
        Ok(enabled) => Ok(enabled),
        Err(e) => Err(format!("Failed to check autostart status: {}", e)),
    }
}

#[tauri::command]
async fn get_system_proxy_status(_state: State<'_, AppState>) -> Result<bool, String> {
    match ech_workers_client_lib::sysproxy::is_system_proxy_enabled() {
        Ok(enabled) => Ok(enabled),
        Err(e) => Err(format!("Failed to check system proxy status: {}", e)),
    }
}

fn main() {
    // Initialize configuration
    let config = Config::load().unwrap_or_else(|e| {
        log::error!("Failed to load config: {}, using defaults", e);
        Config::default()
    });

    log::info!("Application starting");

    let app_state = AppState {
        tray: std::sync::Mutex::new(None),
        proxy_server: std::sync::Mutex::new(None),
        config: tokio::sync::Mutex::new(config),
        exiting: std::sync::atomic::AtomicBool::new(false),
    };

    log::info!("Initializing Tauri application");

    tauri::Builder::default()
        .manage(app_state)
        .invoke_handler(tauri::generate_handler![
            start_proxy,
            stop_proxy,
            get_proxy_status,
            get_config,
            save_config,
            get_servers,
            get_current_server,
            set_current_server,
            upsert_server,
            delete_server,
            get_system_proxy_status,
            get_autostart_status,
            set_system_proxy,
            set_autostart,
            get_proxy_output,
            get_last_state,
            clear_proxy_output
        ])
        .setup(|app| {
            log::info!("Setting up tray");
            tray::create_tray(app.app_handle())?;

            let app_handle = app.app_handle().clone();
            let window = app.get_webview_window("main").unwrap();

            window.clone().on_window_event(move |event| {
                match event {
                    tauri::WindowEvent::CloseRequested { api, .. } => {
                        let state = app_handle.state::<AppState>();
                        if state.exiting.load(std::sync::atomic::Ordering::SeqCst) {
                            // 允许正常关闭
                            log::info!("Allowing application to close as exit flag is set");
                        } else {
                            log::info!("Window close requested, hiding to tray");
                            api.prevent_close();
                            if let Err(e) = tray::hide_window_to_tray(&window) {
                                log::error!("Failed to hide window to tray: {}", e);
                            }
                        }
                    }
                    _ => {}
                }
            });

            {
                let app_handle = app.app_handle().clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(tokio::time::Duration::from_secs(3)).await;
                    let state = app_handle.state::<AppState>();
                    let cfg;
                    let last;
                    {
                        let guard = state.config.lock().await;
                        cfg = guard.get_proxy_config();
                        last = guard.get_last_state();
                    }
                    if check_external_process_running().await {
                        return;
                    }
                    if last.was_running {
                        if let Err(e) = start_proxy(state.clone(), cfg.clone()).await {
                            log::error!("Failed to auto-start proxy: {}", e);
                        } else if last.system_proxy_enabled {
                            if let Err(e) = set_system_proxy(true, state.clone()).await {
                                log::error!("Failed to auto-enable system proxy: {}", e);
                            }
                        }
                    } else if last.system_proxy_enabled {
                        if let Err(e) = set_system_proxy(true, state.clone()).await {
                            log::error!("Failed to auto-enable system proxy: {}", e);
                        }
                    }
                });
            }

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}

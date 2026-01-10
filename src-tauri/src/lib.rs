// Prevents additional console window on Windows in release, DO NOT REMOVE!!
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

pub mod external_proxy;
pub mod config;
pub mod sysproxy;
pub mod autostart;
pub mod tray;

use tauri::tray::TrayIcon;
use external_proxy::{ExternalProxyServer, get_output, clear_output};
use config::Config;
use std::sync::Mutex;
use tauri::{Manager, State};

pub struct AppState {
    pub tray: Mutex<Option<TrayIcon>>,
    pub proxy_server: Mutex<Option<ExternalProxyServer>>,
    pub config: tokio::sync::Mutex<Config>,
    pub exiting: std::sync::atomic::AtomicBool,
}

#[tauri::command]
async fn start_proxy(
    state: State<'_, AppState>,
    config: config::ProxyConfig,
) -> Result<String, String> {

    // 如检测到外部进程已在运行，则直接返回成功信息并同步状态
    if check_external_process_running().await {
        {
            let mut config_guard = state.config.lock().await;
            let mut last_state = config_guard.get_last_state();
            last_state.was_running = true;
            config_guard.set_last_state(last_state);
            let _ = config_guard.save();
        }
        return Ok("External proxy server already running externally".to_string());
    }

    // ===== 第 1 段：同步检查（无 await）=====
    {
        let guard = state
            .proxy_server
            .lock()
            .map_err(|_| "Proxy mutex poisoned")?;

        if guard.is_some() {
            return Err("External proxy server is already running".to_string());
        }
        // guard 在这里自动 drop
    }

    // ===== 第 2 段：异步启动（无 MutexGuard）=====
    let mut server = ExternalProxyServer::new(config)
        .map_err(|e| format!("Failed to create proxy server: {}", e))?;

    server
        .start()
        .await
        .map_err(|e| format!("Failed to start proxy server: {}", e))?;

    // ===== 第 3 段：同步写回（无 await）=====
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
    log::info!("stop_proxy called");

    let mut server = {
        let mut guard = state
            .proxy_server
            .lock()
            .map_err(|_| "Proxy mutex poisoned")?;

        guard.take()   // 取出并 drop guard
    };

    if let Some(server) = server.as_mut() {
        log::info!("Stopping proxy server...");
        server
            .stop()
            .await
            .map_err(|e| format!("Failed to stop proxy server: {}", e))?;

        log::info!("Proxy server stopped, now disabling system proxy...");

        // 停止代理时同时关闭系统代理 - 使用项目的 set_system_proxy 函数
        match crate::set_system_proxy(false, state.clone()).await {
            Ok(_) => {
                log::info!("System proxy disabled successfully via set_system_proxy");
            }
            Err(e) => {
                log::error!("Failed to disable system proxy via set_system_proxy: {}", e);
                return Err(format!("Failed to disable system proxy: {}", e));
            }
        }

        Ok("External proxy server stopped successfully".to_string())
    } else {
        log::warn!("No proxy server was running");

        // 即使没有代理服务器在运行，也要尝试禁用系统代理
        log::info!("Attempting to disable system proxy anyway...");
        match crate::set_system_proxy(false, state.clone()).await {
            Ok(_) => {
                log::info!("System proxy disabled successfully via set_system_proxy");
                let mut config_guard = state.config.lock().await;
                let mut last_state = config_guard.get_last_state();
                last_state.was_running = false;
                config_guard.set_last_state(last_state);
                let _ = config_guard.save();
                Ok("System proxy disabled successfully".to_string())
            }
            Err(e) => {
                log::error!("Failed to disable system proxy via set_system_proxy: {}", e);
                Err(format!("Failed to disable system proxy: {}", e))
            }
        }
    }
}

/// 检查系统中是否有 ech-workers.exe 进程在运行
async fn check_external_process_running() -> bool {
    #[cfg(windows)]
    {
        use std::process::Command;
        use std::os::windows::process::CommandExt;

        let output = Command::new("tasklist")
            .args(&["/FI", "IMAGENAME eq ech-workers.exe", "/FO", "CSV"])
            .creation_flags(0x08000000)
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
        false
    }
}


#[tauri::command]
async fn get_proxy_status(state: State<'_, AppState>) -> Result<bool, String> {
    let guard = state
        .proxy_server
        .lock()
        .map_err(|_| "Proxy mutex poisoned")?;

    Ok(guard.is_some())
}


#[tauri::command]
async fn get_config(state: State<'_, AppState>) -> Result<config::ProxyConfig, String> {
    let config_state = state.config.lock().await;
    Ok(config_state.get_proxy_config())
}

#[tauri::command]
async fn save_config(
    state: State<'_, AppState>,
    config: config::ProxyConfig,
) -> Result<(), String> {
    let mut config_state = state.config.lock().await;
    config_state.set_proxy_config(config);
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

        sysproxy::set_system_proxy(host, port)
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
        sysproxy::disable_system_proxy()
            .map_err(|e| format!("Failed to disable system proxy: {}", e))?;

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
    Ok(())
}

#[tauri::command]
async fn set_autostart(enabled: bool) -> Result<(), String> {
    if enabled {
        autostart::enable_autostart()
            .map_err(|e| format!("Failed to enable autostart: {}", e))?;
    } else {
        autostart::disable_autostart()
            .map_err(|e| format!("Failed to disable autostart: {}", e))?;
    }
    Ok(())
}

#[tauri::command]
async fn get_proxy_output() -> Result<Vec<String>, String> {
    Ok(get_output())
}

#[tauri::command]
async fn clear_proxy_output() -> Result<(), String> {
    clear_output();
    Ok(())
}

pub async fn cleanup_all_processes() {
    #[cfg(windows)]
    {
        use std::process::Command;
        use std::os::windows::process::CommandExt;
        let _ = Command::new("taskkill")
            .args(&["/F", "/IM", "ech-workers.exe", "/T"])
            .creation_flags(0x08000000) // CREATE_NO_WINDOW
            .output();
    }
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    env_logger::init();

    // Initialize configuration
    let config = Config::load().unwrap_or_else(|e| {
        log::error!("Failed to load config: {}, using defaults", e);
        Config::default()
    });

    let app_state = AppState {
        tray: Mutex::new(None),
        proxy_server: Mutex::new(None).into(),
        config: tokio::sync::Mutex::new(config),
        exiting: std::sync::atomic::AtomicBool::new(false),
    };

    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .manage(app_state)
        .invoke_handler(tauri::generate_handler![
            start_proxy,
            stop_proxy,
            get_proxy_status,
            get_config,
            save_config,
            set_system_proxy,
            set_autostart,
            get_proxy_output,
            clear_proxy_output
        ])
        .setup(|app| {
            tray::create_tray(app.app_handle())?;

            // 设置窗口关闭事件 - 隐藏到托盘而不是退出
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

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}

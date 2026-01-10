use tauri::{
    AppHandle, Manager, WebviewWindow,
    menu::{Menu, MenuItem, PredefinedMenuItem, CheckMenuItem},
    tray::{TrayIconBuilder, TrayIconEvent, MouseButton},
};

pub fn create_tray(app: &AppHandle) -> Result<(), Box<dyn std::error::Error>> {
    // 创建托盘菜单
    let start_proxy_item = MenuItem::new(app, "启动代理", true, Some("start_proxy"))?;
    let stop_proxy_item = MenuItem::new(app, "停止代理", false, Some("stop_proxy"))?;
    let sys_enabled = crate::sysproxy::is_system_proxy_enabled().unwrap_or(false);
    let toggle_system_proxy_item = CheckMenuItem::new(app, "系统代理", sys_enabled, true, Some("toggle_system_proxy"))?;
    let separator = PredefinedMenuItem::separator(app)?;
    let quit_item = MenuItem::new(app, "退出", true, Some("quit"))?;

    let menu = Menu::with_items(app, &[
        &start_proxy_item,
        &stop_proxy_item,
        &toggle_system_proxy_item,
        &separator,
        &quit_item,
    ])?;

    // 创建托盘图标
    let _tray_icon = TrayIconBuilder::new()
        .menu(&menu)
        .show_menu_on_left_click(false)
        .icon(app.default_window_icon().unwrap().clone())
        .tooltip("ECH-Workers 客户端")
        .build(app)?;

    {
        let start_item = start_proxy_item.clone();
        let stop_item = stop_proxy_item.clone();
        let sys_item = toggle_system_proxy_item.clone();
        let handle = app.app_handle().clone();
        tauri::async_runtime::spawn(async move {
            loop {
                let state = handle.state::<crate::AppState>();
                if state.exiting.load(std::sync::atomic::Ordering::SeqCst) {
                    break;
                }
                let is_managed_running = {
                    let guard = state.proxy_server.lock().map(|g| g.is_some()).unwrap_or(false);
                    guard
                };
                let _ = start_item.set_enabled(!is_managed_running);
                let _ = stop_item.set_enabled(is_managed_running);
                let sys_enabled = crate::sysproxy::is_system_proxy_enabled().unwrap_or(false);
                let _ = sys_item.set_checked(sys_enabled);
                tokio::time::sleep(std::time::Duration::from_millis(1000)).await;
            }
        });
    }

    // 处理托盘图标点击事件
    let app_handle = app.clone();
    app.on_tray_icon_event(move |_tray_id, event| {
        match event {
            TrayIconEvent::Click { button, .. } if button == MouseButton::Left => {
                if let Some(window) = app_handle.get_webview_window("main") {
                    if window.is_visible().unwrap_or(false) {
                        let _ = window.hide();
                    } else {
                        let _ = window.show();
                        let _ = window.set_focus();
                    }
                }
            }
            _ => {}
        }
    });

    // 处理菜单事件
    let app_handle = app.clone();
    app.on_menu_event(move |_window, event| {
        let _window = app_handle.get_webview_window("main").unwrap();

        match event.id.as_ref() {
            "start_proxy" => {
                log::info!("Start proxy requested from tray menu");
                let handle = app_handle.clone();
                let start_item = start_proxy_item.clone();
                let stop_item = stop_proxy_item.clone();
                tauri::async_runtime::spawn(async move {
                    let state = handle.state::<crate::AppState>();
                    let cfg = {
                        let config_guard = state.config.lock().await;
                        config_guard.get_proxy_config()
                    };
                    let _ = crate::start_proxy(state, cfg).await;
                    let _ = stop_item.set_enabled(true);
                    let _ = start_item.set_enabled(false);
                });
            }
            "stop_proxy" => {
                log::info!("Stop proxy requested from tray menu");
                let handle = app_handle.clone();
                let start_item = start_proxy_item.clone();
                let stop_item = stop_proxy_item.clone();
                tauri::async_runtime::spawn(async move {
                    let state = handle.state::<crate::AppState>();
                    let _ = crate::stop_proxy(state).await;
                    let _ = stop_item.set_enabled(false);
                    let _ = start_item.set_enabled(true);
                });
            }
            "toggle_system_proxy" => {
                log::info!("Toggle system proxy requested from tray menu");
                let handle = app_handle.clone();
                let sys_item = toggle_system_proxy_item.clone();
                tauri::async_runtime::spawn(async move {
                    let state = handle.state::<crate::AppState>();
                    let enabled = crate::sysproxy::is_system_proxy_enabled().unwrap_or(false);
                    let target = !enabled;
                    let _ = crate::set_system_proxy(target, state).await;
                    let _ = sys_item.set_checked(target);
                });
            }
            "quit" => {
                log::info!("=== Quit menu item clicked ===");
                let handle = app_handle.clone();
                // 先设置退出标志，防止窗口关闭事件阻止退出
                {
                    let state = handle.state::<crate::AppState>();
                    state.exiting.store(true, std::sync::atomic::Ordering::SeqCst);
                    log::info!("Exit flag set to true");
                }
                // 使用 block_on 同步执行清理操作，然后退出
                log::info!("Starting synchronous cleanup...");
                let handle_clone = handle.clone();
                tauri::async_runtime::block_on(async {
                    log::info!("Cleanup task started");
                    let state = handle_clone.state::<crate::AppState>();
                    log::info!("Stopping proxy...");
                    let _ = crate::stop_proxy(state.clone()).await;
                    log::info!("Disabling system proxy...");
                    let _ = crate::set_system_proxy(false, state.clone()).await;
                    log::info!("Cleaning up processes...");
                    crate::cleanup_all_processes().await;
                    // 关闭所有窗口
                    log::info!("Closing windows...");
                    if let Some(window) = handle_clone.get_webview_window("main") {
                        let _ = window.close();
                        log::info!("Main window closed");
                    }
                    for (_, w) in handle_clone.webview_windows() {
                        let _ = w.close();
                    }
                    log::info!("Cleanup completed");
                });
                // 清理完成后，直接退出
                log::info!("Exiting application with handle.exit(0)...");
                handle.exit(0);
                log::info!("Calling std::process::exit(0) as fallback...");
                std::process::exit(0);
            }
            _ => {}
        }
    });

    Ok(())
}

pub fn hide_window_to_tray(
    window: &WebviewWindow,
) -> Result<(), Box<dyn std::error::Error>> {
    window.hide()?;
    Ok(())
}

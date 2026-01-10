use anyhow::{Result, anyhow};
use log::{info, warn};
use winreg::enums::*;
use winreg::RegKey;

pub fn set_system_proxy(host: &str, port: &str) -> Result<()> {
    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    
    // Set proxy server
    let proxy_settings = hkcu.open_subkey_with_flags(
        "Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings",
        KEY_ALL_ACCESS
    ).map_err(|e| anyhow!("Failed to open Internet Settings registry key: {}", e))?;

    let proxy_server = format!("{}:{}", host, port);
    proxy_settings.set_value("ProxyServer", &proxy_server)
        .map_err(|e| anyhow!("Failed to set ProxyServer registry value: {}", e))?;

    proxy_settings.set_value("ProxyEnable", &1u32)
        .map_err(|e| anyhow!("Failed to enable proxy: {}", e))?;

    // Notify system of proxy change
    unsafe {
        use windows::Win32::UI::WindowsAndMessaging::SendMessageTimeoutW;
        use windows::Win32::UI::WindowsAndMessaging::HWND_BROADCAST;
        use windows::Win32::UI::WindowsAndMessaging::WM_SETTINGCHANGE;
        use windows::Win32::UI::WindowsAndMessaging::SMTO_NORMAL;
        use windows::core::HSTRING;
        use windows::Win32::Foundation::WPARAM;
        use windows::Win32::Foundation::LPARAM;

        let settings = HSTRING::from("Internet Settings");
        let result = SendMessageTimeoutW(
            HWND_BROADCAST,
            WM_SETTINGCHANGE,
            WPARAM(0),
            LPARAM(settings.as_ptr() as isize),
            SMTO_NORMAL,
            5000,
            None,
        );
        
        if result.0 == 0 {
            warn!("Failed to broadcast proxy settings change");
        }
    }

    info!("System proxy set to {}:{}", host, port);
    Ok(())
}

pub fn disable_system_proxy() -> Result<()> {
    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    
    let proxy_settings = hkcu.open_subkey_with_flags(
        "Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings",
        KEY_ALL_ACCESS
    ).map_err(|e| anyhow!("Failed to open Internet Settings registry key: {}", e))?;

    // 禁用代理
    proxy_settings.set_value("ProxyEnable", &0u32)
        .map_err(|e| anyhow!("Failed to disable proxy: {}", e))?;

    // 可选：清除代理服务器设置
    proxy_settings.delete_value("ProxyServer")
        .ok(); // 忽略删除失败的错误，因为可能不存在

    // 清除其他可能的代理设置
    proxy_settings.delete_value("AutoConfigURL")
        .ok(); // 忽略删除失败的错误

    info!("System proxy disabled via registry");

    // Notify system of proxy change
    unsafe {
        use windows::Win32::UI::WindowsAndMessaging::SendMessageTimeoutW;
        use windows::Win32::UI::WindowsAndMessaging::HWND_BROADCAST;
        use windows::Win32::UI::WindowsAndMessaging::WM_SETTINGCHANGE;
        use windows::Win32::UI::WindowsAndMessaging::SMTO_NORMAL;
        use windows::core::HSTRING;
        use windows::Win32::Foundation::WPARAM;
        use windows::Win32::Foundation::LPARAM;

        let settings = HSTRING::from("Internet Settings");
        let result = SendMessageTimeoutW(
            HWND_BROADCAST,
            WM_SETTINGCHANGE,
            WPARAM(0),
            LPARAM(settings.as_ptr() as isize),
            SMTO_NORMAL,
            5000,
            None,
        );
        
        if result.0 == 0 {
            warn!("Failed to broadcast proxy settings change");
        }
    }

    // 额外：使用 PowerShell 命令强制禁用系统代理
    use std::process::Command;
    use std::os::windows::process::CommandExt;
    
    let powershell_result = Command::new("powershell")
        .args(&["-Command", "Set-ItemProperty -Path 'HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings' -Name ProxyEnable -Value 0"])
        .creation_flags(0x08000000) // CREATE_NO_WINDOW
        .output();

    match powershell_result {
        Ok(output) => {
            if !output.status.success() {
                warn!("PowerShell proxy disable command failed: {}", String::from_utf8_lossy(&output.stderr));
            } else {
                info!("PowerShell proxy disable command executed successfully");
            }
        }
        Err(e) => {
            warn!("Failed to execute PowerShell proxy disable command: {}", e);
        }
    }

    info!("System proxy disabled");
    Ok(())
}

#[allow(dead_code)]
pub fn is_system_proxy_enabled() -> Result<bool> {
    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    
    let proxy_settings = hkcu.open_subkey_with_flags(
        "Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings",
        KEY_READ
    ).map_err(|e| anyhow!("Failed to open Internet Settings registry key: {}", e))?;

    match proxy_settings.get_value::<u32, _>("ProxyEnable") {
        Ok(value) => Ok(value != 0),
        Err(_) => Ok(false),
    }
}

#[allow(dead_code)]
pub fn get_system_proxy() -> Result<(String, u16)> {
    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    
    let proxy_settings = hkcu.open_subkey_with_flags(
        "Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings",
        KEY_READ
    ).map_err(|e| anyhow!("Failed to open Internet Settings registry key: {}", e))?;

    let proxy_server: String = proxy_settings.get_value("ProxyServer")
        .map_err(|e| anyhow!("Failed to get ProxyServer value: {}", e))?;

    // Parse host:port format
    if let Some((host, port_str)) = proxy_server.split_once(':') {
        let port = port_str.parse::<u16>()
            .map_err(|e| anyhow!("Invalid proxy port: {}", e))?;
        Ok((host.to_string(), port))
    } else {
        Err(anyhow!("Invalid proxy server format: {}", proxy_server))
    }
}

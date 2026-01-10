use anyhow::{Result, anyhow};
use log::info;
use winreg::enums::*;
use winreg::RegKey;
use std::path::PathBuf;
use std::env;

pub fn enable_autostart() -> Result<()> {
    let current_exe = env::current_exe()
        .map_err(|e| anyhow!("Failed to get current executable path: {}", e))?;

    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    let path = PathBuf::from("SOFTWARE")
        .join("Microsoft")
        .join("Windows")
        .join("CurrentVersion")
        .join("Run");

    let autostart_key = hkcu.open_subkey_with_flags(path, KEY_ALL_ACCESS)
        .map_err(|e| anyhow!("Failed to open autostart registry key: {}", e))?;

    let app_name = "ECH Client";
    autostart_key.set_value(app_name, &current_exe.to_string_lossy().to_string())
        .map_err(|e| anyhow!("Failed to set autostart registry value: {}", e))?;

    info!("Autostart enabled for {:?}", current_exe);
    Ok(())
}

pub fn disable_autostart() -> Result<()> {
    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    let path = PathBuf::from("SOFTWARE")
        .join("Microsoft")
        .join("Windows")
        .join("CurrentVersion")
        .join("Run");

    let autostart_key = hkcu.open_subkey_with_flags(path, KEY_ALL_ACCESS)
        .map_err(|e| anyhow!("Failed to open autostart registry key: {}", e))?;

    let app_name = "ECH Client";
    autostart_key.delete_value(app_name)
        .map_err(|e| anyhow!("Failed to delete autostart registry value: {}", e))?;

    info!("Autostart disabled");
    Ok(())
}

#[allow(dead_code)]
pub fn is_autostart_enabled() -> Result<bool> {
    let current_exe = env::current_exe()
        .map_err(|e| anyhow!("Failed to get current executable path: {}", e))?;

    let hkcu = RegKey::predef(HKEY_CURRENT_USER);
    let path = PathBuf::from("SOFTWARE")
        .join("Microsoft")
        .join("Windows")
        .join("CurrentVersion")
        .join("Run");

    let autostart_key = hkcu.open_subkey_with_flags(path, KEY_READ)
        .map_err(|e| anyhow!("Failed to open autostart registry key: {}", e))?;

    let app_name = "ECH Client";
    match autostart_key.get_value::<String, _>(app_name) {
        Ok(stored_path) => {
            let normalized_stored = stored_path.to_lowercase().replace("\"", "");
            let normalized_current = current_exe.to_string_lossy().to_lowercase().replace("\"", "");
            Ok(normalized_stored == normalized_current)
        }
        Err(_) => Ok(false),
    }
}

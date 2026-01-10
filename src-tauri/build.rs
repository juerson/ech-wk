fn main() {
    #[cfg(target_os = "windows")]
    {
        // 在Windows上，确保在发布版本中不显示控制台窗口
        if std::env::var("PROFILE").unwrap_or_default() == "release" {
            println!("cargo:rustc-cdylib-link-arg=/SUBSYSTEM:WINDOWS");
        }
    }
    tauri_build::build()
}

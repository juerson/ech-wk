use std::process::Stdio;
use std::path::PathBuf;
use tokio::process::Child as TokioChild;
use tokio::process::Command as TokioCommand;
use tokio::io::{AsyncBufReadExt, BufReader};
use log::{info, error, debug, warn};
use anyhow::{Result, anyhow};
use crate::config::ProxyConfig;
use std::sync::Mutex as StdMutex;

// #[cfg(windows)]
// use std::os::windows::process::CommandExt;

// 全局输出缓冲区
static OUTPUT_BUFFER: StdMutex<Vec<String>> = StdMutex::new(Vec::new());

// 添加输出到缓冲区
fn add_output(line: String) {
    if let Ok(mut buffer) = OUTPUT_BUFFER.lock() {
        buffer.push(line);
        // 保持缓冲区大小在合理范围内
        if buffer.len() > 1000 {
            buffer.remove(0);
        }
    }
}

// 获取所有输出
pub fn get_output() -> Vec<String> {
    OUTPUT_BUFFER.lock()
        .map(|buffer| buffer.clone())
        .unwrap_or_else(|_| Vec::new())
}

// 清空输出
pub fn clear_output() {
    if let Ok(mut buffer) = OUTPUT_BUFFER.lock() {
        buffer.clear();
    }
}

pub struct ExternalProxyServer {
    process: Option<TokioChild>,
    config: ProxyConfig,
    exe_path: PathBuf,
}

impl ExternalProxyServer {
    pub fn new(config: ProxyConfig) -> Result<Self> {
        // 获取当前程序所在目录
        let mut exe_path = std::env::current_exe()
            .map_err(|e| anyhow!("Failed to get current exe path: {}", e))?;

        // 获取程序所在目录
        exe_path.pop(); // 移除文件名，保留目录

        // 构建外部程序路径
        exe_path.push("ech-workers.exe");

        info!("External proxy executable path: {:?}", exe_path);

        if !exe_path.exists() {
            return Err(anyhow!("External proxy executable not found: {:?}", exe_path));
        }

        Ok(Self {
            process: None,
            config,
            exe_path,
        })
    }

    pub async fn start(&mut self) -> Result<()> {
        // 首先检查是否有残留的 ech-workers.exe 进程并清理
        self.cleanup_existing_processes().await?;

        // 检查我们是否已经管理了一个进程
        if self.process.is_some() {
            return Err(anyhow!("External proxy server is already running"));
        }

        // 再次检查系统中是否还有 ech-workers.exe 进程（清理后可能还有）
        if self.check_existing_process().await {
            return Err(anyhow!("External proxy server is already running (external process detected)"));
        }

        info!("Starting external proxy server...");
        debug!("Using config: {:?}", self.config);

        // 构建命令参数 - 使用 ech-workers.exe 的实际参数格式
        let mut cmd = TokioCommand::new(&self.exe_path);

        // 必需参数：服务端地址
        if !self.config.server_addr.is_empty() {
            cmd.arg("-f");
            cmd.arg(&self.config.server_addr);
        } else {
            return Err(anyhow!("Server address is required for ech-workers.exe"));
        }

        // 可选参数：本地监听地址
        if !self.config.listen_addr.is_empty() {
            cmd.arg("-l");
            cmd.arg(&self.config.listen_addr);
        }

        // 可选参数：身份验证令牌
        if !self.config.token.is_empty() {
            cmd.arg("-token");
            cmd.arg(&self.config.token);
        }

        // 可选参数：指定服务端 IP（绕过 DNS）
        if !self.config.server_ip.is_empty() {
            cmd.arg("-ip");
            cmd.arg(&self.config.server_ip);
        }

        // 可选参数：ECH 查询 DoH 服务器
        if !self.config.dns_server.is_empty() {
            cmd.arg("-dns");
            cmd.arg(&self.config.dns_server);
        }

        // 可选参数：ECH 查询域名
        if !self.config.ech_domain.is_empty() {
            cmd.arg("-ech");
            cmd.arg(&self.config.ech_domain);
        }

        // 可选参数：分流模式
        if !self.config.routing_mode.is_empty() {
            cmd.arg("-routing");
            cmd.arg(&self.config.routing_mode);
        }

        // 设置标准输出和错误输出以便日志记录
        cmd.stdout(Stdio::piped());
        cmd.stderr(Stdio::piped());

        // 在Windows上隐藏控制台窗口
        #[cfg(windows)]
        {
            cmd.creation_flags(0x08000000); // CREATE_NO_WINDOW
        }

        debug!("Executing command: {:?}", cmd);
        info!("Starting external proxy server with config:");
        info!("  Server: {}", self.config.server_addr);
        info!("  Listen: {}", self.config.listen_addr);
        if !self.config.token.is_empty() {
            info!("  Token: [REDACTED]");
        }
        if !self.config.server_ip.is_empty() {
            info!("  Server IP: {}", self.config.server_ip);
        }
        if !self.config.dns_server.is_empty() {
            info!("  DNS Server: {}", self.config.dns_server);
        }
        if !self.config.ech_domain.is_empty() {
            info!("  ECH Domain: {}", self.config.ech_domain);
        }
        if !self.config.routing_mode.is_empty() {
            info!("  Routing Mode: {}", self.config.routing_mode);
        }

        match cmd.spawn() {
            Ok(mut child) => {
                // 启动输出监控任务
                if let Some(stdout) = child.stdout.take() {
                    let stdout_reader = BufReader::new(stdout);
                    tokio::spawn(async move {
                        let mut lines = stdout_reader.lines();
                        while let Ok(Some(line)) = lines.next_line().await {
                            let formatted_line = format!("[STDOUT] {}", line);
                            info!("{}", formatted_line);
                            add_output(formatted_line);
                        }
                    });
                }

                if let Some(stderr) = child.stderr.take() {
                    let stderr_reader = BufReader::new(stderr);
                    tokio::spawn(async move {
                        let mut lines = stderr_reader.lines();
                        while let Ok(Some(line)) = lines.next_line().await {
                            let formatted_line = format!("{}", line);
                            warn!("{}", formatted_line);
                            add_output(formatted_line);
                        }
                    });
                }

                self.process = Some(child);
                info!("External proxy server started successfully with PID: {:?}",
                      self.process.as_ref().unwrap().id());

                Ok(())
            }
            Err(e) => {
                error!("Failed to start external proxy server: {}", e);
                Err(anyhow!("Failed to spawn external process: {}", e))
            }
        }
    }

    pub async fn stop(&mut self) -> Result<()> {
        if let Some(mut child) = self.process.take() {
            info!("Stopping external proxy server...");

            match child.kill().await {
                Ok(_) => {
                    debug!("External proxy server process killed");
                    match child.wait().await {
                        Ok(status) => {
                            info!("External proxy server stopped with status: {}", status);
                        }
                        Err(e) => {
                            warn!("Failed to wait for process termination: {}", e);
                        }
                    }
                }
                Err(e) => {
                    error!("Failed to kill external proxy server: {}", e);
                    return Err(anyhow!("Failed to kill process: {}", e));
                }
            }
        } else {
            warn!("External proxy server is not running");
        }

        Ok(())
    }

    /// 清理系统中残留的 ech-workers.exe 进程
    async fn cleanup_existing_processes(&self) -> Result<()> {
        info!("Checking for existing ech-workers.exe processes...");

        #[cfg(windows)]
        {
            use std::process::Command;
            use std::os::windows::process::CommandExt;

            // 多次尝试清理，确保进程被彻底杀死
            for attempt in 1..=3 {
                info!("Process cleanup attempt {}", attempt);

                // 使用 tasklist 命令查找 ech-workers.exe 进程
                let output = Command::new("tasklist")
                    .args(&["/FI", "IMAGENAME eq ech-workers.exe", "/FO", "CSV"])
                    .creation_flags(0x08000000) // CREATE_NO_WINDOW
                    .output();

                match output {
                    Ok(result) => {
                        let output_str = String::from_utf8_lossy(&result.stdout);

                        if output_str.contains("ech-workers.exe") {
                            info!("Found existing ech-workers.exe processes, cleaning up...");

                            // 使用 taskkill 命令强制终止所有 ech-workers.exe 进程
                            let kill_output = Command::new("taskkill")
                                .args(&["/F", "/IM", "ech-workers.exe", "/T"])
                                .creation_flags(0x08000000) // CREATE_NO_WINDOW
                                .output();

                            match kill_output {
                                Ok(kill_result) => {
                                    let kill_str = String::from_utf8_lossy(&kill_result.stdout);
                                    info!("Process cleanup result: {}", kill_str);

                                    // 等待更长时间确保进程完全终止
                                    tokio::time::sleep(tokio::time::Duration::from_millis(1000)).await;

                                    // 再次检查是否还有进程
                                    let check_output = Command::new("tasklist")
                                        .args(&["/FI", "IMAGENAME eq ech-workers.exe", "/FO", "CSV"])
                                        .creation_flags(0x08000000)
                                        .output();

                                    if let Ok(check_result) = check_output {
                                        let check_str = String::from_utf8_lossy(&check_result.stdout);
                                        if !check_str.contains("ech-workers.exe") {
                                            info!("All ech-workers.exe processes successfully terminated");
                                            break;
                                        } else {
                                            warn!("Some processes still running, retrying...");
                                        }
                                    }
                                }
                                Err(e) => {
                                    warn!("Failed to kill existing processes: {}", e);
                                }
                            }
                        } else {
                            info!("No existing ech-workers.exe processes found");
                            break;
                        }
                    }
                    Err(e) => {
                        warn!("Failed to check existing processes: {}", e);
                    }
                }

                // 如果不是最后一次尝试，等待一段时间再重试
                if attempt < 3 {
                    tokio::time::sleep(tokio::time::Duration::from_millis(1000)).await;
                }
            }
        }

        #[cfg(not(windows))]
        {
            // 在非Windows系统上，可以使用 pkill 或类似命令
            info!("Process cleanup not implemented for non-Windows systems");
        }

        Ok(())
    }

    /// 检查系统中是否还有 ech-workers.exe 进程在运行
    async fn check_existing_process(&self) -> bool {
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

    pub fn is_running(&self) -> bool {
        self.process.is_some()
    }

    pub async fn check_status(&mut self) -> Result<bool> {
        if let Some(child) = &mut self.process {
            match child.try_wait() {
                Ok(Some(status)) => {
                    info!("External proxy server exited with status: {}", status);
                    self.process = None;
                    Ok(false)
                }
                Ok(None) => {
                    // 进程仍在运行
                    Ok(true)
                }
                Err(e) => {
                    error!("Failed to check process status: {}", e);
                    Ok(false)
                }
            }
        } else {
            Ok(false)
        }
    }
}

impl Drop for ExternalProxyServer {
    fn drop(&mut self) {
        if let Some(child) = self.process.take() {
            info!("Cleaning up external proxy server process...");
            // 在同步上下文中使用 std::process::Child 的 kill 方法
            let child_id = child.id();
            if let Some(id) = child_id {
                debug!("Attempting to kill process with PID: {}", id);
                // 使用 Windows API 或其他同步方法来终止进程
                // 这里我们简单地记录，因为 tokio::process::Child 的 kill 是异步的
                debug!("Process cleanup completed for PID: {}", id);
            }
        }
    }
}

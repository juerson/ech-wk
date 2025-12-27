# ECH Workers 客户端

跨平台的 ECH Workers 代理客户端，支持 Windows 和 macOS。

## 功能特性

- ✅ 服务器配置管理（支持多个服务器配置）
- ✅ 启动/停止代理进程
- ✅ 实时日志显示
- ✅ 系统托盘图标（可选）
- ✅ 开机自动启动
- ✅ 配置自动保存

## 快速开始

### 方法 1: 使用预编译版本（推荐）

从 [GitHub Releases](https://github.com/your-repo/releases) 下载对应平台的压缩包：

- **Windows**: `ECHWorkers-windows-amd64.zip`
- **macOS Intel**: `ECHWorkers-darwin-amd64.zip`
- **macOS Apple Silicon**: `ECHWorkers-darwin-arm64.zip`

解压后：
1. 安装 Python 3.6+（如果还没有）
2. 安装依赖: `pip install PyQt5`
3. 运行: `python gui.py` 或使用启动脚本

### 方法 2: 从源码编译

#### 编译 Go 程序

```bash
# 初始化 Go 模块
go mod init ech-workers
go mod tidy

# 编译
go build -o ech-workers ech-workers.go
```

#### 运行 Python 客户端

```bash
# 安装依赖
pip install PyQt5

# 运行
python3 gui.py
```

## 文件说明

### 核心文件
- `ech-workers.go` - Go 源码（核心代理程序）
- `gui.py` - Python GUI 客户端（使用 PyQt5）

### 配置文件
- `go.mod`, `go.sum` - Go 模块文件
- `requirements.txt` - Python 依赖列表

## 系统要求

- **Windows**: Windows 10+
- **macOS**: macOS 10.13+
- **Python**: 3.6+
- **Go**: 1.23+ (仅编译时需要，ECH 功能需要此版本)

## 配置说明

配置文件保存在：
- **Windows**: `%APPDATA%\ECHWorkersClient\config.json`
- **macOS**: `~/Library/Application Support/ECHWorkersClient/config.json`
- **Linux**: `~/.config/ECHWorkersClient/config.json`

## 使用说明

1. **配置服务器**
   - 点击"新增"添加服务器配置
   - 填写服务地址和监听地址
   - 可选：填写身份令牌、IP、DNS、ECH 等高级选项

2. **启动代理**
   - 点击"启动代理"按钮
   - 查看日志区域了解运行状态

3. **系统托盘**（如果安装了 pystray）
   - 应用会在菜单栏显示图标
   - 点击图标可以打开/隐藏窗口

## 故障排除

### PyQt5 安装问题

如果遇到 PyQt5 安装问题：
```bash
# macOS
pip3 install --user PyQt5
# 或
pip3 install --break-system-packages PyQt5

# Windows
pip install PyQt5
```

### 找不到 ech-workers

确保已编译 Go 程序：
```bash
go build -o ech-workers ech-workers.go
```

## 开发

### 本地测试

```bash
# 编译 Go
go build -o ech-workers ech-workers.go

# 测试 Python
python3 -m py_compile gui.py
```

### GitHub Actions

推送标签会自动触发构建和发布：
```bash
git tag v1.0.0
git push origin v1.0.0
```

## 许可证

[添加你的许可证]

## 贡献

欢迎提交 Issue 和 Pull Request！


# ECH Workers Client (Go GUI)

> **注意**: 
>
> 1、本项目仅适配 Windows x64 架构（AMD64），不支持其他操作系统或架构，代码没有完善(没有对应的开发环境，懒得弄了)。
>
> 2、支持外部ech-workers.exe以及内嵌的ech-workers，外部的ech-workers.exe文件没有才使用内部的。

本文档介绍如何将 ECH Workers 客户端编译为 Windows 可执行文件 (.exe)。

## 前置要求

### 1. 安装 Go

确保已安装 Go 1.18 或更高版本：
```bash
go version
```

### 2. 安装 GCC (必需)

Windows 平台需要 GCC 编译器来支持 CGO。

#### 方法一：使用 MSYS2（推荐）

1. **下载 MSYS2**
   - 访问 [https://www.msys2.org/](https://www.msys2.org/)
   - 下载 `msys2-x86_64-20231026.exe` 或最新版本
   - 运行安装程序，按默认设置安装

2. **安装 MinGW-w64 工具链**
   ```bash
   # 打开 MSYS2 UCRT64 终端
   pacman -S --needed mingw-w64-ucrt-x86_64-toolchain
   ```

3. **配置环境变量**
   将 MSYS2 的 bin 目录添加到系统 PATH：
   ```
   C:\msys64\ucrt64\bin
   ```

4. **验证安装**
   ```bash
   gcc --version
   ```

#### 方法二：使用 TDM-GCC

1. 下载 [TDM-GCC](https://jmeubank.github.io/tdm-gcc/)
2. 安装时选择 64-bit 版本
3. 自动配置环境变量

### 3. 设置 CGO 环境变量

```powershell
$env:CGO_ENABLED="1"
```

## 依赖管理

### 主要依赖

```go
// go.mod
module github.com/juerson/ech-wk/client-gui-go

go 1.25.5

require (
    fyne.io/fyne/v2 v2.7.1           // GUI 框架
    github.com/gorilla/websocket v1.5.3 // WebSocket 支持
    golang.org/x/sys v0.30.0          // 系统调用
)
```

### 间接依赖

项目依赖以下关键库：

| 库名 | 版本 | 用途 |
|------|------|------|
| fyne.io/systray | v1.11.1 | 系统托盘支持 |
| github.com/go-gl/glfw/v3.3/glfw | v0.0.0-20240506104042 | OpenGL 窗口管理 |
| github.com/fyne-io/oksvg | v0.2.0 | SVG 图像处理 |
| github.com/go-text/typesetting | v0.2.1 | 文本排版 |
| github.com/fsnotify/fsnotify | v1.9.0 | 文件系统监控 |
| github.com/godbus/dbus/v5 | v5.1.0 | Linux D-Bus 通信 |
| golang.org/x/image | v0.24.0 | 图像处理 |
| golang.org/x/net | v0.35.0 | 网络库 |
| golang.org/x/text | v0.22.0 | 文本处理 |

### 依赖安装

在编译前，确保所有依赖都已下载：

```bash
# 清理模块缓存（可选）
go clean -modcache

# 下载依赖
go mod download

# 验证依赖
go mod verify

# 整理依赖
go mod tidy
```

### 常见依赖问题

#### 1. 网络超时
```bash
# 设置代理（如果需要）
go env -w GOPROXY=https://goproxy.cn,direct
go env -w GOSUMDB=sum.golang.org
```

#### 2. 版本冲突
```bash
# 更新依赖
go get -u ./...

# 或指定版本
go get fyne.io/fyne/v2@v2.7.1
```

#### 3. CGO 依赖问题
某些 Fyne 依赖需要 CGO，确保：
- Windows：安装 GCC
- macOS：安装 Xcode Command Line Tools
- Linux：安装 gcc 和相关开发包

```bash
# macOS
xcode-select --install

# Ubuntu/Debian
sudo apt-get install gcc libc6-dev libgl1-mesa-dev libx11-dev libxrandr-dev libxinerama-dev libxcursor-dev libxi-dev libglfw3-dev

# CentOS/RHEL
sudo yum install gcc mesa-libGL-devel libX11-devel libXrandr-devel libXinerama-devel libXcursor-devel libXi-devel glfw-devel
```

## 编译流程

### 1. 准备图标文件（可选）

如果需要为程序添加图标：

1. 准备 `.ico` 格式图标文件（建议 256x256 像素）
2. 将图标文件放在项目根目录

### 2. 安装资源编译工具

```bash
go install github.com/akavel/rsrc@latest
```

### 3. 生成资源文件

```bash
# 在项目根目录生成
rsrc -ico app.ico -o rsrc.syso
```

### 4. 编译程序

#### 基础编译
```bash
go build -o ech-client.exe .
```

#### 优化编译（推荐）
```bash
# 启用优化并隐藏控制台窗口
go build -ldflags "-s -w -H=windowsgui" -o ech-client.exe .
```

#### 完整编译命令（带图标）
```bash
# 1. 设置环境变量
$env:CGO_ENABLED="1"

# 2. 生成资源文件
rsrc -ico app.ico -o rsrc.syso

# 3. 编译
go build -ldflags "-s -w -H=windowsgui" -o ech-client.exe .
```

## 编译参数说明

| 参数 | 说明 |
|------|------|
| `-ldflags "-s -w"` | 去除调试信息，减小文件大小 |
| `-H=windowsgui` | 隐藏控制台窗口，作为 GUI 应用运行 |
| `-o ech-client.exe` | 指定输出文件名 |

## 常见问题

### 1. CGO 相关错误

**错误：** `cgo: C compiler "gcc" not found: exec: "gcc": executable file not found`

**解决：** 确保已正确安装 GCC 并配置了环境变量。

### 2. 资源文件错误

**错误：** `rsrc: command not found`

**解决：** 运行 `go install github.com/akavel/rsrc@latest` 安装工具。

### 3. 图标不显示

**解决：** 
- 确保图标文件为 `.ico` 格式
- 检查图标文件路径是否正确
- 重新生成资源文件并编译

## 项目结构

```
client-gui-go/
├── main.go                  # 主程序入口
├── core/                    # 核心代理功能
│   └── ech-workers.go       # ECH Workers 代理实现
├── internal/                # 内部模块
│   ├── autostart/           # 开机启动
│   ├── config/              # 配置管理
│   ├── process/             # 进程管理
│   ├── sysproxy/            # 系统代理
│   └── ui/                  # UI 界面
├── test/                    # 测试套件
├── tools/                   # 工具脚本
├── go.mod
├── go.sum
├── app.ico                  # 应用图标
└── README.md
```

## 运行程序

编译完成后，双击 `ech-client.exe` 即可运行程序。程序支持：

- 系统托盘图标
- 开机自启动
- 系统代理设置
- 多服务器配置管理

## 技术栈

- **GUI 框架：** Fyne v2.7+
- **编程语言：** Go 1.18+
- **系统支持：** Windows x64 (AMD64) 仅支持
- **编译器：** GCC (Windows CGO)

## 许可证

本项目采用 MIT 许可证。详见 LICENSE 文件。

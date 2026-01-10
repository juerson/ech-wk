# ECH Workers Client - Windows 编译流程

> 适用于 Windows x64 (AMD64) 架构

## 1. 环境准备

### 安装 Go 语言
```bash
# 下载并安装 Go 1.25.5 或更高版本
# https://golang.org/dl/ 下载 go1.25.5.windows-amd64.msi

# 安装完成后，验证安装
go version
```

### 安装 GCC 编译器
```bash
# 下载并安装 MSYS2
# https://www.msys2.org/ 下载 msys2-x86_64-20251213.exe

# 安装完成后，打开 MSYS2 UCRT64 终端，运行：
pacman -S --needed mingw-w64-ucrt-x86_64-toolchain

# 添加到系统 PATH 环境变量：
# C:\msys64\ucrt64\bin
```

### Fyne GUI 依赖说明
本项目使用 Fyne GUI 框架，需要以下库支持：
- OpenGL (已包含在 mingw-w64-ucrt-x86_64-toolchain 中)
- X11 相关库 (已包含在 mingw-w64-ucrt-x86_64-toolchain 中)

如遇到 GUI 相关编译错误，可手动安装：
```bash
# 在 MSYS2 UCRT64 终端中运行
pacman -S mingw-w64-ucrt-x86_64-mesa
pacman -S mingw-w64-ucrt-x86_64-glfw
```

### 设置环境变量
```powershell
$env:CGO_ENABLED="1"
```

## 2. 获取代码

```bash
git clone https://github.com/juerson/ech-wk.git
cd ech-wk/client-gui-go
```

## 3. 安装依赖

```bash
go mod download
go mod tidy
```

## 4. 编译程序

### 基础编译
```bash
go build -o ech-client.exe .
```

### 优化编译（推荐）
```bash
go build -ldflags "-s -w -H=windowsgui" -o ech-client.exe .
```

## 5. 添加图标（可选）

### 安装图标工具
```bash
go install github.com/akavel/rsrc@latest
```

### 生成资源文件
```bash
# 确保项目根目录有 app.ico 文件
rsrc -ico app.ico -o rsrc.syso
```

### 带图标编译
```bash
go build -ldflags "-H=windowsgui" -o ech-client.exe .
go build -ldflags "-s -w -H=windowsgui" -o ech-client.exe .
```

## 6. 验证编译

编译完成后，会生成 `ech-client.exe` 文件。双击运行即可。

## 常见问题

### GCC 未找到
确保已安装 MSYS2 并将 `C:\msys64\ucrt64\bin` 添加到 PATH。

### 编译失败
检查 Go 版本是否为 1.18+，确保网络连接正常以下载依赖。

### Fyne GUI 编译错误
如果遇到 OpenGL 或 GLFW 相关错误：
```bash
# 在 MSYS2 UCRT64 终端中运行
pacman -S mingw-w64-ucrt-x86_64-mesa
pacman -S mingw-w64-ucrt-x86_64-glfw
```

### 图标不显示
确保 app.ico 文件存在且格式正确，重新生成 rsrc.syso 文件。
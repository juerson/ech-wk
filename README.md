## Ech-Workers Python版的客户端

基于原作者v1.4版本修改，支持开机启动自动启动代理，适配2k小屏，敏感数据脱敏。

## 编译方法

### 方法一：PyInstaller (推荐新手)

**特点：**
- ✅ 单个exe文件，分发方便
- ✅ 兼容性好，支持所有Python版本
- ❌ 文件较大 (37.5MB)
- ❌ 启动稍慢
- ✅ 支持开机启动

**编译步骤：**
```cmd
# 1. 创建虚拟环境
python -m venv env

# 2. 激活虚拟环境（Windows CMD）
env\Scripts\activate

# 3. 安装依赖
pip install -r requirements.txt
pip install pyinstaller

# 4. 编译
pyinstaller --onefile --windowed --name gui gui.py

# 5. 完成！可执行文件在 dist/gui.exe，还要复制一个“ech-workers.exe”进去就可以执行
```

### 方法二：Nuitka (推荐高级用户)

**特点：**
- ✅ 文件更小 (6.1MB主程序)
- ✅ 性能更好，启动更快
- ✅ C编译优化
- ❌ 需要整个文件夹
- ❌ 编译时间较长
- ❌ 不支持开机启动

**编译步骤：**
```cmd
# 1. 使用Python 3.12 (重要！)
python --version  # 确认是3.12版本

# 2. 创建虚拟环境
python -m venv env_nuitka

# 3. 激活虚拟环境
env_nuitka\Scripts\activate

# 4. 安装依赖
pip install -r requirements.txt
pip install --upgrade nuitka

# 5. 编译
python -m nuitka --standalone --enable-plugin=pyqt5 --windows-disable-console --output-filename=gui.exe --assume-yes-for-downloads gui.py

# 6. 完成！可执行文件在 gui.dist/gui.exe (需要整个文件夹)，还要复制一个“ech-workers.exe”进去就可以执行
```

## 常见问题与解决方案

### 问题1：PyInstaller编译失败 - 找不到resources目录
**错误：** `ERROR: Unable to find 'resources' when adding binary and data files`
**解决：** 修改gui.spec文件，将 `datas=[('resources', 'resources')]` 改为 `datas=[]`

### 问题2：Nuitka编译失败 - Python版本不兼容
**错误：** `LINK : fatal error LNK1104: cannot open file 'python313t.lib'`
**解决：** 使用Python 3.11或3.12，不要使用3.13

### 问题3：Nuitka缺少依赖工具
**错误：** 编译时提示下载Dependency Walker
**解决：** 添加 `--assume-yes-for-downloads` 参数

### 问题4：编译后exe无法运行
**原因：** 缺少PyQt5等依赖
**解决：** 确保在安装依赖后重新编译

## 输出文件对比

| 编译方式 | 文件位置 | 文件大小 | 特点 |
|---------|---------|---------|------|
| PyInstaller | `dist/gui.exe` | 37.5MB | 单文件，方便分发 |
| Nuitka | `gui.dist/gui.exe` | 6.1MB + 依赖 | 性能更好，需整个文件夹 |

## 推荐选择

- **个人使用** → Nuitka版本 (性能更好)
- **分发他人** → PyInstaller版本 (更方便)
- **首次编译** → PyInstaller版本 (更简单)


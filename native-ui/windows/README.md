# UU Link Windows x64 预览

这是一款 Windows 10/11 x64 的 TCP/UDP 端口转发控制端。远端继续运行支持相应协议的 UU Link 服务。当前没有 ARM64 原生包，也不包含 Windows 被控端。

- 从安装包安装后在开始菜单打开 **UU Link**。安装不要求管理员权限，缺少 WebView2 时会联网安装 [Microsoft Evergreen Runtime](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution)。
- 便携版请完整解压后打开 **UU Link.exe**，并保留同目录的 **uulink-engine.exe**。若没有 WebView2，请先安装 Microsoft Runtime。
- 应用默认只允许本机访问映射。为单条映射开启“允许局域网访问”后，还需要 Windows 防火墙允许所需端口；建议仅允许信任的专用网络。
- UU 连接只使用 Windows 报告为物理硬件、具有 IPv4 默认路由的以太网或 Wi-Fi；无合适接口时连接会失败，不回退到普通路由或代理。第三方驱动和 Windows 网络过滤器仍可能阻断流量。
- 关闭窗口会停止转发。登录时启动由当前 Windows 用户的登录项管理，可在应用设置关闭。卸载保留 `%LOCALAPPDATA%\UU Link` 中的本机账户和映射配置。
- 首个预览未签名。下载后可用 `Get-FileHash -Algorithm SHA256` 与发行页的摘要文件核对。

## 实机验收清单

使用测试映射和一个支持相应协议的远端。不要让 Windows 与现有客户端同时连接同一个正在使用的 UU 隧道，以免影响已有连接。

1. 在 Windows x64 上安装和打开，检查标题栏、图标、窗口缩放，以及切换系统浅色与深色后的外观。关闭再打开，检查主题和映射设置是否保留。
2. 用首次登录的身份发送一次短信验证码，输入验证码后确认设备列表。记录失败的 HTTP 状态、错误码和界面提示；不要共享手机号、验证码或身份文件。
3. 新建两条映射，使用实际 TCP 和 UDP 服务验证数据收发。增删或停用其中一条，确认另一条仍能收发；再断开并恢复 Windows 网络，观察恢复情况。
4. 用第二台局域网设备测试一条映射：关闭“允许局域网访问”时无法连接，开启后在防火墙允许的情况下可以收发。在线设备可测 UU 隧道延迟，离线设备不应显示测量按钮。
5. 从便携目录执行下列只读诊断。开启和关闭系统代理或虚拟网卡分别检查，预期 UU 连接使用同一块可用物理网卡，且没有物理路由时报告错误。诊断不会读取账号或启用转发。

   ```powershell
   .\uulink-engine.exe version
   .\uulink-engine.exe network-check --udp --ice --timeout 35s
   ```

6. 开启防止自动休眠后运行 `powercfg /requests`，关闭软件后再检查请求是否释放。确认 Windows 登录项关闭、开启及再次登录后的行为。
7. 关闭软件，确认没有 `uulink-engine.exe` 残留、映射端口不再监听。对测试配置重复一次，结束 **UU Link.exe** 进程，检查 Job Object 也能清理子进程。最后卸载，确认开始菜单和安装文件清除。

## 构建

在 macOS 或 Linux 上准备 Go、ImageMagick、NSIS、curl、zip 和 shasum，从仓库根目录执行：

```bash
./native-ui/windows/package.sh ./dist
```

这个命令从源码生成两种 Windows x64 分发包与 SHA256 文件。Windows 应用源代码由根模块的 `gui_*_windows.go`、`gui_assets/` 和独立的连接引擎模块 `experiments/udp-game-adaptive/` 组成。MS WebView2 Runtime 是桌面界面的外部运行依赖。

当前验证包括交叉构建及安装包内容检查。上述 Windows 实机行为需要单独验收。

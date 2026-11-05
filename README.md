# UU Link

通用 TCP + UDP 端口转发工具。macOS 软件管理连接与多条映射，Linux 服务端接收转发；两端使用同一个 UU 账号。桌面客户端 **v0.3.2**，Linux 安装脚本固定 **v0.3.1**，协议引擎 **0.5.0-portmap-exp1**。

## Windows x64 预览

[安装版、便携版和校验文件](https://github.com/ct-jyjntc/uulink/releases/tag/v0.3.5-windows.1) · [构建与 Windows 实机验收清单](native-ui/windows/README.md)

Windows 版沿用通用 TCP/UDP 映射、每条规则的局域网开关、设备列表和隧道延迟。它是控制端，适用于 Windows 10/11 x64。首次预览未签名，Windows 安装、短信登录、转发和退出清理等待实机验收。

## macOS

在 [Releases](https://github.com/ct-jyjntc/uulink/releases/tag/v0.3.2) 下载 `UU-Link-0.3.2-macos-arm64.zip`，解压后打开 UU Link。支持 Apple Silicon，声明最低 macOS 13；本机构建使用 ad-hoc 签名，未做 Apple 公证，其他 Mac 安装仍需实测。

0.3.2 修正右上角和侧栏齿轮图标的比例、居中与裁切，保留正确的 24×24 图标画布。

0.3.1 修复首次登录设备注册缺少 `mac` 导致的 HTTP 422；重新设计图标，修正透明留白和显示占比。支持默认 TCP+UDP、多个映射并行、每条规则的局域网开关、真实 UU RTT、系统主题、登录自启和防休眠。

## Linux 服务端一键安装

适用于 **全新 Linux amd64 / arm64 + systemd**，需预装 Bash、curl、系统 CA、sha256sum 与 awk。在 SSH 交互终端执行一行：

```bash
( uulink_setup=$(mktemp) && trap 'rm -f -- "$uulink_setup"' EXIT && curl -fsSL https://raw.githubusercontent.com/ct-jyjntc/uulink/v0.3.1/install.sh -o "$uulink_setup" && bash "$uulink_setup" )
```

普通用户会通过 sudo 提权；root 无需 sudo。脚本校验固定版本的 SHA256，安装纯转发服务并配置该服务的 IPv4 地址族限制，然后启动并配置开机自启。首次安装按提示输入手机号和短信验证码，身份仅保存在服务器本地。登录同一 UU 账户后，在 Mac 软件设备页选中该服务器添加映射；远端目标是服务器可访问的地址，例如 `127.0.0.1:22`。

脚本拒绝覆盖已有身份、规则或服务。OpenRC 暂不由此脚本安装。不修改整机 IPv6、路由或防火墙，不启用远程终端、文件管理、桌面或电源控制。若登录中断或安装失败，已写出的身份保留，可能需按错误提示检查后恢复；不自动清空配置。

可先下载脚本审阅，或运行 `bash install.sh --help` / `--plan`。`--check` 只检查本地安装前置条件。安装完成后：`sudo uulink status`、`sudo uulink doctor`、`sudo uulink logs`。

## 验证范围

设备注册的原始请求已复现 422，补齐 `mac` 后真实接口返回 200。现有 amd64 服务已跑通通用 TCP+UDP、HTTPS、UDP DNS、真实 RTT 和服务重启恢复。图标主体占比与本机系统图标相同。两个 Linux 架构均为静态构建；arm64 尚未实机运行。

安装脚本已做语法、参数、摘要解析与现有 Linux 部署拒绝覆盖检查；本轮未在空白服务器完成真实短信登录安装，也未验证整机重启、长期离线及长时间 UDP 稳定性。`active/enabled` 不等同于端到端连通或实际重启验收。完整短信登录需使用本人的有效手机号和验证码，程序不会在测试中替他人发送短信。

发布附件包含 SHA256SUMS，第三方许可见 [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt)。

#!/usr/bin/env bash
# Fresh controlled-side install only. Release binaries are built/published separately.
set -euo pipefail
set +x
export LC_ALL=C
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

usage() {
    cat <<'HELP'
UU 通用转发服务端安装（Linux amd64 / arm64，systemd，全新安装）
用法: sudo bash install.sh [选项]
  --repo OWNER/REPO    GitHub 发布仓库（默认 ct-jyjntc/uulink）
  --tag TAG            固定 Release tag（默认 v0.3.1；不使用 latest）
  --sha256 HEX        可选：从独立可信渠道获得的本架构二进制 SHA256
  --plan              仅打印下载 URL 和 CLI 参数；无需 Linux/root/终端，无网络/写入
  --check             仅做本地前置检查；需 Linux/root，无网络/写入，不要求终端
  -h, --help          显示帮助，无网络/写入

资产：uulink-server-linux-amd64 / uulink-server-linux-arm64；摘要：SHA256SUMS
固定 CLI: install --components mapping --disable-power --skip-deps --yes --relay --no-start
先安装不启动，再为 uulink.service 添加 IPv4 限制，最后启动。
首次登录由 CLI 从 /dev/tty 交互读取本人手机号及短信验证码。
已有身份、规则、服务或安装目录会被拒绝；不提供升级/覆盖/自动卸载。
依赖须预装：bash curl sha256sum awk、系统 CA、带 seccomp 支持的 systemd。
CLI 本身支持 OpenRC，但本脚本因无法等效强制进程 IPv4 而在其前置阶段拒绝。
仅安装转发组件，禁用远程电源操作，不修改全局 IPv6/路由/防火墙。
失败清理临时下载和自有锁，并尝试停止/禁用本次新服务；保留身份及安装文件。
HELP
}

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1（请先自行安装依赖）"; }
exists() { [[ -e "$1" || -L "$1" ]]; }

validate_release() {
    [[ "$repo" != *REPLACE* && "$tag" != *REPLACE* ]] ||
        die '请替换发布仓库和 tag 占位参数；不会下载占位 URL。'
    [[ "$repo" =~ ^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ||
        die '--repo 必须是 OWNER/REPO，不接受 URL、空白或路径穿越。'
    [[ "$tag" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && "$tag" != latest ]] ||
        die '--tag 必须是固定 tag（仅字母、数字、点、下划线、连字符）。'
    [[ -z "$pinned_sha" || "$pinned_sha" =~ ^[[:xdigit:]]{64}$ ]] ||
        die '--sha256 必须是 64 位十六进制。'
}

select_arch() {
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die '仅支持 Linux amd64/x86_64 与 arm64/aarch64。' ;;
    esac
}

assert_fresh() {
    local p
    # Refuse entire identity/rule directories, including dangling symlinks and
    # custom binary/service layouts. Never read or print credential contents.
    for p in /etc/uulink /etc/uulink-controller \
        /usr/local/lib/uulink /usr/local/lib/uulink-controller \
        /usr/local/bin/uulink /usr/local/bin/uulink-controller \
        /var/lib/uulink /var/log/uulink \
        /run/uulink /run/uulink-client /run/uulink-controller /run/uulink.pid \
        /etc/init.d/uulink /etc/init.d/uulink-controller \
        /etc/conf.d/uulink /etc/conf.d/uulink-controller \
        /etc/systemd/system/uulink.service /etc/systemd/system/uulink.service.d \
        /etc/systemd/system/uulink-controller.service /etc/systemd/system/uulink-controller.service.d \
        /run/systemd/system/uulink.service /run/systemd/system/uulink.service.d \
        /run/systemd/system/uulink-controller.service /run/systemd/system/uulink-controller.service.d \
        /usr/lib/systemd/system/uulink.service /lib/systemd/system/uulink.service \
        /usr/lib/systemd/system/uulink-controller.service /lib/systemd/system/uulink-controller.service; do
        exists "$p" && die "发现已有路径 $p；为保留身份、规则与服务，拒绝覆盖。"
    done
    return 0
}

check_manager() {
    local state unit
    # Match platform.go detection order; additionally require a live manager.
    if [[ -d /run/systemd/system ]]; then
        manager=systemd
    elif command -v openrc-run >/dev/null 2>&1 && command -v rc-service >/dev/null 2>&1; then
        manager=openrc
    elif command -v systemctl >/dev/null 2>&1; then
        manager=systemd
    else
        die '需要正在运行的 systemd 或 OpenRC；不支持只有工具的容器/chroot。'
    fi
    if [[ "$manager" == systemd ]]; then
        need systemctl
        systemctl show --property=Version >/dev/null || die 'systemd 管理器不可用。'
        state=$(systemctl --version) || die '无法读取 systemd 构建信息。'
        [[ " $state " == *'+SECCOMP'* && -r /proc/sys/kernel/seccomp/actions_avail ]] ||
            die '无法确认 systemd/内核具备 seccomp 支持，拒绝在 IPv4 限制不明确时安装。'
        for unit in uulink.service uulink-controller.service; do
            state=$(systemctl show --property=LoadState --value "$unit") ||
                die "无法确认 $unit 是否存在；已安全退出。"
            [[ "$state" == not-found ]] || die "systemd 已知 $unit（$state），拒绝覆盖。"
        done
    else
        for unit in openrc-run rc-service rc-update supervise-daemon; do need "$unit"; done
        [[ -d /run/openrc ]] || die '未发现运行中的 OpenRC（/run/openrc）。'
        rc-update show default >/dev/null || die 'OpenRC 管理器不可用。'
        # The CLI-generated init script has this literal shebang.
        [[ -x /sbin/openrc-run ]] || die 'CLI 要求 /sbin/openrc-run 可执行。'
        die '现有 CLI 支持 OpenRC，但没有进程级 IPv4 开关；本脚本不能等效实现 RestrictAddressFamilies，故不安装、不登录。'
    fi
}

preflight() {
    local cmd ca have_ca=0
    [[ "$(uname -s)" == Linux ]] || die '实际安装/--check 仅支持 Linux；可用 --plan 查看计划。'
    if [[ "$EUID" -ne 0 ]]; then
        need sudo
        [[ -f "${BASH_SOURCE[0]:-}" ]] || die '请先将脚本下载为文件，再运行以便 sudo 提权。'
        exec sudo -- bash "${BASH_SOURCE[0]}" "${original_args[@]}"
    fi
    for cmd in curl sha256sum awk tr mktemp mkdir rm rmdir chmod; do need "$cmd"; done
    for ca in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem /etc/pki/tls/certs/ca-bundle.crt; do
        [[ -s "$ca" ]] && have_ca=1
    done
    [[ "$have_ca" -eq 1 ]] || die '缺少系统 CA 证书包；请先安装 ca-certificates。'
    [[ -d /run && ! -L /run && -w /run ]] || die '需要可写且非符号链接的 /run。'
    assert_fresh
    check_manager
}

# Extract exactly one conventional sha256sum entry for the requested asset.
# Never pass an untrusted manifest directly to sha256sum -c (arbitrary paths).
extract_digest() {
    awk -v name="$1" '
        { sub(/\r$/, "") }
        $2 == name || $2 == "*" name {
            count++
            if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) bad=1
            digest=tolower($1)
        }
        END { if (count != 1 || bad) exit 1; print digest }
    '
}

download() {
    # -q first: ignore curlrc; HTTPS remains mandatory through redirects.
    curl -q --fail --location --silent --show-error --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 300 --retry 2 --max-filesize "$3" \
        --output "$2" --url "$1"
}

# systemd may reorder the property. Require exactly the requested allowlist.
check_ipv4_families() {
    awk '{for (i=1; i<=NF; i++) {seen[$i]++; n++}}
         END {exit !(n == 3 && seen["AF_UNIX"] == 1 && seen["AF_INET"] == 1 && seen["AF_NETLINK"] == 1)}'
}

install_ipv4_dropin() {
    local dir=/etc/systemd/system/uulink.service.d families
    # Fresh installation only: refuse even an unexpected empty drop-in directory.
    mkdir -m 0755 -- "$dir" || die 'IPv4 drop-in 目录已存在或无法创建；未覆盖。'
    (set -o noclobber
        printf '[Service]\nRestrictAddressFamilies=\nRestrictAddressFamilies=AF_UNIX AF_INET AF_NETLINK\n' > "$dir/90-ipv4-only.conf"
    ) || die '写入 IPv4 drop-in 失败。'
    chmod 0644 "$dir/90-ipv4-only.conf"
    systemctl daemon-reload || die '加载 IPv4 drop-in 失败；未启动服务。'
    families=$(systemctl show --property=RestrictAddressFamilies --value uulink.service) ||
        die '无法读取服务 IPv4 限制；未启动服务。'
    printf '%s\n' "$families" | check_ipv4_families || die '服务 IPv4 限制读回不符；未启动服务。'
}

cleanup() {
    local rc=$?
    trap - EXIT
    if [[ "$rc" -ne 0 && "$install_started" -eq 1 ]]; then
        # No unit existed before this attempt. --no-start still enables it, so
        # disable our partial service to avoid an unconfined start on next boot.
        if [[ -f /etc/systemd/system/uulink.service && ! -L /etc/systemd/system/uulink.service ]]; then
            systemctl stop uulink.service || printf '警告：停止本次新服务失败，请人工检查。\n' >&2
            systemctl disable uulink.service || printf '警告：禁用本次新服务失败，请人工检查。\n' >&2
        fi
        printf '%s\n' 'CLI 安装已开始；可能已保存身份或部分服务。未删除/卸载/回滚文件，请检查 /etc/uulink 和服务状态后再处理。' >&2
    fi
    if [[ -n "$workdir" ]]; then
        rm -rf -- "$workdir" || printf '警告：临时目录清理失败：%s\n' "$workdir" >&2
    fi
    if [[ "$lock_owned" -eq 1 ]]; then
        rmdir -- "$lockdir" || printf '警告：安装锁清理失败：%s\n' "$lockdir" >&2
    fi
    exit "$rc"
}

main() {
    local repo=ct-jyjntc/uulink tag=v0.3.1 pinned_sha=''
    local mode=install arch asset sums base digest actual cmd
    local manager=''
    # EXIT runs after main returns; these cleanup variables must outlive its locals.
    workdir='' lock_owned=0 install_started=0
    lockdir=/run/uulink-server-install.lock
    local -a original_args=("$@")
    local -a cli_args=(install --components mapping --disable-power --skip-deps --yes --relay --no-start)
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -h|--help) usage; return 0 ;;
            --repo|--tag|--sha256)
                [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 缺少参数。"
                case "$1" in
                    --repo) repo=$2 ;; --tag) tag=$2 ;;
                    --sha256) pinned_sha=$2 ;;
                esac
                shift 2 ;;
            --plan|--check)
                [[ "$mode" == install ]] || die '--plan 与 --check 不能组合或重复。'
                mode=${1#--}; shift ;;
            *) die "未知参数：$1（见 --help）。" ;;
        esac
    done
    validate_release
    select_arch
    asset="uulink-server-linux-$arch"
    sums=SHA256SUMS
    base="https://github.com/$repo/releases/download/$tag"
    printf '二进制：%s/%s\n校验表：%s/%s\nCLI：' "$base" "$asset" "$base" "$sums"
    printf ' %s' "${cli_args[@]}"
    printf '\n'
    printf '%s\n' '后续：新建 uulink.service.d/90-ipv4-only.conf（AF_UNIX AF_INET AF_NETLINK）→ daemon-reload → 读回限制 → start。'
    [[ "$mode" != plan ]] || return 0
    preflight
    printf '本地前置检查完成：%s / %s（不证明协议兼容或登录可用）。\n' "$arch" "$manager"
    [[ "$mode" != check ]] || return 0
    # Use a controlling terminal even if stdin is a pipe, redirected or /dev/null.
    { exec 3<>/dev/tty; } 2>/dev/null || die '需要交互终端 /dev/tty；SSH 请分配 PTY，禁止无人值守短信登录。'
    [[ -t 3 ]] || die '/dev/tty 不是终端。'
    umask 077
    # Atomic lock only serializes instances of this wrapper. No force/unlock mode.
    mkdir -- "$lockdir" || die "无法取得锁 $lockdir；请确认无其他安装，不自动删除他人的锁。"
    lock_owned=1
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP
    workdir=$(mktemp -d /tmp/uulink-server.XXXXXXXX) || die '无法创建私有下载目录。'
    download "$base/$sums" "$workdir/SHA256SUMS" 1048576 || die '下载 SHA256SUMS 失败。'
    digest=$(extract_digest "$asset" < "$workdir/SHA256SUMS") ||
        die 'SHA256SUMS 缺少本架构条目，或条目重复/格式无效。'
    if [[ -n "$pinned_sha" ]]; then
        [[ "$digest" == "$(printf '%s' "$pinned_sha" | tr 'A-F' 'a-f')" ]] ||
            die '独立指定的 SHA256 与 Release 校验表不符。'
    fi
    download "$base/$asset" "$workdir/engine" 134217728 || die '下载引擎失败。'
    actual=$(sha256sum "$workdir/engine") || die '计算 SHA256 失败。'
    actual=${actual%% *}
    [[ "$actual" == "$digest" ]] || die '二进制 SHA256 不符；未执行引擎。'
    printf 'SHA256 校验一致：%s\n' "$actual"
    chmod 0700 "$workdir/engine"
    # version has no install side effects; it is NOT a protocol compatibility check.
    "$workdir/engine" version || die '引擎无法运行；请核对静态 Linux 构建、CPU 与 /tmp 的 noexec 限制。'
    assert_fresh
    check_manager
    unset UU_CONFIG_FILE UU_ICE_POLICY UU_TRACE_FRAMES
    printf '%s\n' '即将安装纯转发/relay 被控端，配置服务 IPv4 限制后启动；请在终端输入本人手机号和短信验证码。' >&3
    install_started=1
    if "$workdir/engine" "${cli_args[@]}" <&3 >&3 2>&3; then
        :
    else
        cmd=$?
        printf 'CLI 安装退出码：%s\n' "$cmd" >&2
        return "$cmd"
    fi
    install_ipv4_dropin
    systemctl start uulink.service || die '启动新服务失败。'
    systemctl is-active --quiet uulink.service || die '服务当前未 active。'
    systemctl is-enabled --quiet uulink.service || die '服务当前未启用。'
    printf '%s\n' '服务当前已启动并配置自启（未验证整机重启、账号在线或 TCP/UDP/RTT 端到端）。' \
        '查看：sudo uulink status；sudo uulink doctor；sudo uulink logs'
}

if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi

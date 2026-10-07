#!/usr/bin/env bash
# 构建 Linux .deb 安装包（把网关装成 systemd 服务）。
#
# 必须在 Debian/Ubuntu 上执行（需要 dpkg-deb）。macOS 与 Windows 上无法交叉产出 deb。
#
# 用法：
#   ./installer/linux/build-deb.sh              # 使用 dist/ 下已有的 amd64 发行包
#   ARCH=arm64 ./installer/linux/build-deb.sh   # 用 arm64 发行包
#
# 与 Windows 安装包的差异（有意为之，不是遗漏）：
#   Windows 是"每用户安装"（凭据在用户目录，LocalSystem 服务会看不到账号池），
#   而 Linux 上服务器场景更常见，所以这里装成系统服务：专用系统用户 + /var/lib 数据目录。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

ARCH="${ARCH:-amd64}"
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "!! ARCH 只支持 amd64 / arm64（当前: $ARCH）" >&2; exit 1 ;;
esac

if ! command -v dpkg-deb >/dev/null 2>&1; then
  echo "!! 未找到 dpkg-deb：deb 只能在 Debian/Ubuntu 上构建" >&2
  exit 1
fi

VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }

# Debian 版本号不允许随意用 prerelease 语法（`-` 会开始 Debian revision），
# 所以这里把 `-` 一律换成 `~`：`~` 在 dpkg 里排序低于正式版，于是
# `0.9.0~slice8 < 0.9.0`，符合预发布版本的语义。
# 当前版本（0.9.1）没有预发布后缀，这一步是恒等变换 —— 保留它是为了以后再
# 打出 `-rc1` / `-sliceN` 这类版本时不用回头改这里。
DEB_VERSION="$(printf '%s' "$VERSION" | sed 's/-/~/g')"

TARBALL="$ROOT/dist/workbuddy-gateway_${VERSION}_linux_${ARCH}.tar.gz"
[ -f "$TARBALL" ] || { echo "!! 找不到 $TARBALL，请先执行 ./scripts/release.sh" >&2; exit 1; }

STAGE="$ROOT/dist/.deb-stage"
PKG="$STAGE/workbuddy-gateway_${DEB_VERSION}_${ARCH}"
rm -rf "$STAGE"
mkdir -p "$PKG/DEBIAN" \
         "$PKG/usr/bin" \
         "$PKG/lib/systemd/system" \
         "$PKG/usr/share/doc/workbuddy-gateway"

echo "==> 版本 $VERSION（deb 版本号 $DEB_VERSION）/ $ARCH"

# ---- 载荷 ----
echo "==> 解压二进制"
TMP="$STAGE/extract"
mkdir -p "$TMP"
tar -xzf "$TARBALL" -C "$TMP"
SRC="$TMP/linux_${ARCH}"
[ -d "$SRC" ] || { echo "!! 发行包结构异常：缺 linux_${ARCH} 目录" >&2; exit 1; }
install -m 0755 "$SRC/workbuddy-gateway" "$PKG/usr/bin/workbuddy-gateway"
install -m 0755 "$SRC/wb-local-agent" "$PKG/usr/bin/wb-local-agent"

install -m 0644 config.example.json "$PKG/usr/share/doc/workbuddy-gateway/config.example.json"
[ -f DEPLOY.md ] && install -m 0644 DEPLOY.md "$PKG/usr/share/doc/workbuddy-gateway/DEPLOY.md"

# ---- systemd 单元（指向系统路径，不是 /opt）----
echo "==> 安装 systemd 单元"
cat > "$PKG/lib/systemd/system/workbuddy-gateway.service" <<'UNIT'
[Unit]
Description=WorkBuddy Gateway (account pool gateway for CodeBuddy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=workbuddy-gateway
Group=workbuddy-gateway
WorkingDirectory=/var/lib/workbuddy-gateway
ExecStart=/usr/bin/workbuddy-gateway serve --addr 127.0.0.1 --port 8317
Restart=always
RestartSec=60
KillSignal=SIGTERM
TimeoutStopSec=20
StandardOutput=journal
StandardError=journal
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
ReadWritePaths=/var/lib/workbuddy-gateway

[Install]
WantedBy=multi-user.target
UNIT

# ---- DEBIAN/control ----
cat > "$PKG/DEBIAN/control" <<CTRL
Package: workbuddy-gateway
Version: ${DEB_VERSION}
Section: net
Priority: optional
Architecture: ${ARCH}
Maintainer: workbuddy-gateway
Installed-Size: $(du -sk "$PKG/usr" | cut -f1)
Description: WorkBuddy / CodeBuddy account pool gateway with a web panel
 OpenAI-compatible local gateway that pools CodeBuddy accounts and serves
 /v1/chat/completions, /v1/responses and /v1/models, with a built-in web
 admin panel, account governance, scheduling and usage statistics.
 .
 Binds 127.0.0.1 by default. It does not terminate TLS; put it behind a
 reverse proxy if you need remote access.
CTRL

# ---- 维护脚本 ----
# postinst：建系统用户与数据目录，启用服务。
# 说明：Debian 政策不鼓励把 adduser 写进 postinst，但本包不引入 debconf 依赖，
# 这类"自建系统用户 + 自管数据目录"的取舍在同类工具里常见，故在文档中如实说明。
cat > "$PKG/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -e

DATA_DIR=/var/lib/workbuddy-gateway
SERVICE=workbuddy-gateway.service

if ! getent group workbuddy-gateway >/dev/null 2>&1; then
    addgroup --system workbuddy-gateway >/dev/null 2>&1 || true
fi
if ! getent passwd workbuddy-gateway >/dev/null 2>&1; then
    adduser --system --ingroup workbuddy-gateway --home "$DATA_DIR" \
        --no-create-home --disabled-login --gecos "WorkBuddy Gateway" \
        workbuddy-gateway >/dev/null 2>&1 || true
fi

# 数据目录里放凭据，权限收紧；已存在时不改动（升级场景不能覆盖用户数据）
if [ ! -d "$DATA_DIR" ]; then
    install -d -o workbuddy-gateway -g workbuddy-gateway -m 0750 "$DATA_DIR"
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    # 只在首次安装时自动启用并启动；升级时保持服务当前的启停状态由用户决定
    if [ "$1" = "configure" ]; then
        systemctl enable "$SERVICE" >/dev/null 2>&1 || true
        systemctl restart "$SERVICE" >/dev/null 2>&1 || true
    fi
fi

echo "workbuddy-gateway: 已安装。把凭据文件放到 $DATA_DIR 后，面板会立即显示账号。"
echo "workbuddy-gateway: 面板 http://127.0.0.1:8317/panel/（启动时先拉模型目录，前约 30 秒不监听端口）"

exit 0
POSTINST

cat > "$PKG/DEBIAN/prerm" <<'PRERM'
#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    if [ "$1" = "remove" ] || [ "$1" = "deconfigure" ]; then
        systemctl stop workbuddy-gateway.service >/dev/null 2>&1 || true
        systemctl disable workbuddy-gateway.service >/dev/null 2>&1 || true
    fi
fi

exit 0
PRERM

# postrm：purge 时才删用户与数据目录。
# 注意默认的 remove 会保留 /var/lib/workbuddy-gateway —— 那里有真实凭据，
# 顺手删掉是不可接受的（`apt purge` 才是明确的删除意图）。
cat > "$PKG/DEBIAN/postrm" <<'POSTRM'
#!/bin/sh
set -e

if [ "$1" = "purge" ]; then
    rm -rf /var/lib/workbuddy-gateway
    if getent passwd workbuddy-gateway >/dev/null 2>&1; then
        deluser --system workbuddy-gateway >/dev/null 2>&1 || true
    fi
    if getent group workbuddy-gateway >/dev/null 2>&1; then
        delgroup --system workbuddy-gateway >/dev/null 2>&1 || true
    fi
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0
POSTRM

chmod 0755 "$PKG/DEBIAN/postinst" "$PKG/DEBIAN/prerm" "$PKG/DEBIAN/postrm"

rm -rf "$TMP"

# ---- 打包 ----
OUT="$ROOT/dist/workbuddy-gateway_${VERSION}_${ARCH}.deb"
rm -f "$OUT"
echo "==> 生成 deb"
# --root-owner-group 让非 root 构建也能得到正确的 root:root 归属
dpkg-deb --root-owner-group --build "$PKG" "$OUT" >/dev/null

rm -rf "$STAGE"

echo "==> 完成: $OUT"
ls -lh "$OUT"
echo ""
echo "安装:   sudo dpkg -i $(basename "$OUT")"
echo "卸载:   sudo dpkg -r workbuddy-gateway     （保留数据目录）"
echo "彻底删: sudo dpkg --purge workbuddy-gateway （连数据目录一起删）"

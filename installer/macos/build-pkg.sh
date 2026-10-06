#!/usr/bin/env bash
# 构建 macOS 安装包：workbuddy-gateway_<版本>_<arch>.pkg 与同名 .dmg。
#
# 必须在 macOS 上执行（需要 pkgbuild / productbuild / hdiutil）。
#
# 用法：
#   ./installer/macos/build-pkg.sh              # 自动识别本机架构
#   ARCH=arm64 ./installer/macos/build-pkg.sh
#
# 与 switch 的 macOS 安装包的差异（如实说明，不是遗漏）：
#   switch 的 dmg 里是一个 .app（Tauri 桌面壳），拖进「应用程序」即可。
#   本项目目前还没有桌面壳（那是切片 9），所以这里产出的是 **.pkg 安装包**：
#   装的是命令行网关 + 一个每用户的 LaunchAgent（后台常驻 + 登录自启），
#   面板通过浏览器访问。等桌面壳落地后，再用它产出 drag-to-Applications 的 dmg。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "!! 本脚本只能在 macOS 上运行" >&2
  exit 1
fi

for tool in pkgbuild productbuild hdiutil; do
  command -v "$tool" >/dev/null 2>&1 || { echo "!! 缺少 $tool" >&2; exit 1; }
done

# 默认按本机架构；交叉构建另一种架构（Apple Silicon 上打 x64 包）也支持，
# 因为我们的二进制是静态编译的，不需要在目标架构上编译。
if [ -z "${ARCH:-}" ]; then
  case "$(uname -m)" in
    arm64) ARCH=arm64 ;;
    x86_64) ARCH=amd64 ;;
    *) echo "!! 无法识别的架构: $(uname -m)" >&2; exit 1 ;;
  esac
fi
case "$ARCH" in
  amd64) RELEASE_SUFFIX="darwin_amd64"; PKG_ARCH="x86_64" ;;
  arm64) RELEASE_SUFFIX="darwin_arm64"; PKG_ARCH="arm64" ;;
  *) echo "!! ARCH 只支持 amd64 / arm64（当前: $ARCH）" >&2; exit 1 ;;
esac

VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }

TARBALL="$ROOT/dist/workbuddy-gateway_${VERSION}_${RELEASE_SUFFIX}.tar.gz"
[ -f "$TARBALL" ] || { echo "!! 找不到 $TARBALL，请先执行 ./scripts/release.sh" >&2; exit 1; }

IDENTIFIER="com.workbuddy.gateway"
STAGE="$ROOT/dist/.pkg-stage"
ROOTFS="$STAGE/root"
SCRIPTS="$STAGE/scripts"
rm -rf "$STAGE"
# /usr/local/bin 是"运维工具"的惯例位置；两个二进制必须放在同一目录，
# 因为网关按「与自己同目录」查找本机代理。
mkdir -p "$ROOTFS/usr/local/bin" "$ROOTFS/usr/local/share/doc/workbuddy-gateway" "$SCRIPTS"

echo "==> 版本 $VERSION / $ARCH"

echo "==> 解压二进制"
TMP="$STAGE/extract"
mkdir -p "$TMP"
tar -xzf "$TARBALL" -C "$TMP"
SRC="$TMP/$RELEASE_SUFFIX"
[ -d "$SRC" ] || { echo "!! 发行包结构异常：缺 $RELEASE_SUFFIX 目录" >&2; exit 1; }
install -m 0755 "$SRC/workbuddy-gateway" "$ROOTFS/usr/local/bin/workbuddy-gateway"
install -m 0755 "$SRC/wb-local-agent" "$ROOTFS/usr/local/bin/wb-local-agent"

install -m 0644 config.example.json "$ROOTFS/usr/local/share/doc/workbuddy-gateway/config.example.json"
[ -f DEPLOY.md ] && install -m 0644 DEPLOY.md "$ROOTFS/usr/local/share/doc/workbuddy-gateway/DEPLOY.md"
rm -rf "$TMP"

# ---- postinstall：数据目录 + 每用户 LaunchAgent ----
cat > "$SCRIPTS/postinstall" <<'POSTINSTALL'
#!/bin/sh
# 安装后脚本（以 root 运行）。
#
# 关键点：LaunchAgent 必须装到**登录用户**的 ~/Library/LaunchAgents 下。
# pkg 以 root 运行，$HOME 指向 /var/root，所以要用 /dev/console 反查真实用户。
set -e

BIN=/usr/local/bin/workbuddy-gateway
LABEL=com.workbuddy.gateway
PORT=8317

console_user="$(stat -f%Su /dev/console 2>/dev/null || echo "")"
if [ -z "$console_user" ] || [ "$console_user" = "root" ]; then
    echo "workbuddy-gateway: 未检测到登录用户，跳过 LaunchAgent 安装。"
    echo "workbuddy-gateway: 可在登录后手动运行 $BIN serve 启动网关。"
    exit 0
fi

home="$(dscl . -read "/Users/$console_user" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
if [ -z "$home" ] || [ ! -d "$home" ]; then
    echo "workbuddy-gateway: 无法确定 $console_user 的家目录，跳过 LaunchAgent 安装。"
    exit 0
fi

data_dir="$home/.wb-gateway"
agent_dir="$home/Library/LaunchAgents"
plist="$agent_dir/$LABEL.plist"

mkdir -p "$data_dir/logs" "$agent_dir"
chown -R "$console_user" "$data_dir" "$agent_dir"

cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN</string>
        <string>serve</string>
        <string>--addr</string><string>127.0.0.1</string>
        <string>--port</string><string>$PORT</string>
        <string>--auth-dir</string><string>$data_dir</string>
    </array>
    <!-- 工作目录必须是已存在的绝对路径：网关把凭据、config.json、缓存与统计写在 cwd -->
    <key>WorkingDirectory</key><string>$data_dir</string>
    <key>RunAtLoad</key><true/>
    <!-- 只在异常退出时重启；正常退出（例如用户主动停）不重启 -->
    <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
    <key>StandardOutPath</key><string>$data_dir/logs/gateway.log</string>
    <key>StandardErrorPath</key><string>$data_dir/logs/gateway.err.log</string>
</dict>
</plist>
PLIST

chown "$console_user" "$plist"

uid="$(id -u "$console_user" 2>/dev/null || echo "")"
if [ -n "$uid" ]; then
    # bootstrap 到该用户的 GUI 会话；失败不阻断安装（例如无人图形会话时）
    launchctl bootout "gui/$uid/$LABEL" >/dev/null 2>&1 || true
    launchctl bootstrap "gui/$uid" "$plist" >/dev/null 2>&1 || \
        echo "workbuddy-gateway: LaunchAgent 已安装但未能立即加载，重新登录后生效。"
fi

echo "workbuddy-gateway: 已安装到 /usr/local/bin"
echo "workbuddy-gateway: 数据目录 $data_dir"
echo "workbuddy-gateway: 面板 http://127.0.0.1:$PORT/panel/（启动时先拉模型目录，前约 30 秒不监听端口）"
echo "workbuddy-gateway: 关闭自启： launchctl bootout gui/$uid/$LABEL && rm '$plist'"

exit 0
POSTINSTALL

chmod 0755 "$SCRIPTS/postinstall"

# ---- 组件包 → 产品包 ----
COMPONENT="$STAGE/component.pkg"
echo "==> pkgbuild"
pkgbuild --root "$ROOTFS" \
         --scripts "$SCRIPTS" \
         --identifier "$IDENTIFIER" \
         --version "$VERSION" \
         --install-location / \
         "$COMPONENT" >/dev/null

PKG="$ROOT/dist/workbuddy-gateway_${VERSION}_${ARCH}.pkg"
rm -f "$PKG"
echo "==> productbuild"
productbuild --package "$COMPONENT" --identifier "$IDENTIFIER" --version "$VERSION" "$PKG" >/dev/null

# ---- dmg ----
# dmg 里放 pkg 与一份说明：没有 .app 就没有"拖进应用程序"这一步，
# 但保持 dmg 这个外壳，双击挂载后看到安装包与说明，与常见 macOS 分发一致。
DMG_ROOT="$STAGE/dmg"
mkdir -p "$DMG_ROOT"
cp "$PKG" "$DMG_ROOT/"
cat > "$DMG_ROOT/README.txt" <<EOF
WorkBuddy Gateway ${VERSION} (${PKG_ARCH})

Install: double-click $(basename "$PKG")

The package installs:
  /usr/local/bin/workbuddy-gateway   the gateway (panel embedded)
  /usr/local/bin/wb-local-agent      optional local agent
  ~/Library/LaunchAgents/com.workbuddy.gateway.plist   (starts at login)

Data and credentials live in ~/.wb-gateway
Panel: http://127.0.0.1:8317/panel/

Notes
  1. The package is not signed or notarized. If Gatekeeper blocks it, open it with
     Control-click > Open, or run: xattr -d com.apple.quarantine <file>.pkg
  2. At startup the gateway pulls the model catalog before it begins listening, so
     the panel answers after roughly 30 seconds.
  3. To disable autostart:
       launchctl bootout gui/$(id -u)/com.workbuddy.gateway
       rm ~/Library/LaunchAgents/com.workbuddy.gateway.plist
  4. Uninstall: sudo rm /usr/local/bin/workbuddy-gateway /usr/local/bin/wb-local-agent
     (your ~/.wb-gateway data is left untouched)
EOF

DMG="$ROOT/dist/workbuddy-gateway_${VERSION}_${ARCH}.dmg"
rm -f "$DMG"
echo "==> 生成 dmg"
hdiutil create -volname "WorkBuddy Gateway" -srcfolder "$DMG_ROOT" -ov -format UDZO "$DMG" >/dev/null

rm -rf "$STAGE"

echo "==> 完成"
ls -lh "$PKG" "$DMG"
echo ""
echo "安装: open $(basename "$DMG")  然后双击里面的 .pkg"

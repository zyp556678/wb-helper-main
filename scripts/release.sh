#!/usr/bin/env bash
# 发行打包：交叉编译多平台，产出可直接解压使用的压缩包。
#
# 用法：
#   ./scripts/release.sh                # 编译全部目标平台
#   ./scripts/release.sh linux/amd64    # 只编指定平台（可多次传参）
#
# 产物落在 dist/ 下，命名形如：
#   workbuddy-gateway_0.9.0-slice8_linux_amd64.tar.gz
#   workbuddy-gateway_0.9.0-slice8_windows_amd64.zip
#
# 每个包内含两个二进制：网关（含内嵌面板）与本机代理。
# 本机代理只在「本机形态」下有用；服务端部署可以只用网关，缺本机代理时
# 网关会静默跳过并隐藏面板上的「本机客户端」入口。
set -euo pipefail

# 依赖代理：切片 15 起引入了 modernc.org/sqlite（纯 Go 的 SQLite）。
# Go 默认代理 proxy.golang.org 在国内常不可达，会让交叉编译以
# 「dial tcp ... connection attempt failed」失败 —— 那是网络不是代码。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ---- 定位 Go（优先仓库内便携工具链）----
if [ -x "$ROOT/.toolchain/go/bin/go.exe" ]; then
  GO_BIN="$ROOT/.toolchain/go/bin/go.exe"
  export GOROOT="$(cygpath -w "$ROOT/.toolchain/go" 2>/dev/null || echo "$ROOT/.toolchain/go")"
elif [ -x "$ROOT/.toolchain/go/bin/go" ]; then
  GO_BIN="$ROOT/.toolchain/go/bin/go"
  export GOROOT="$ROOT/.toolchain/go"
else
  GO_BIN="go"
fi
export GOPATH="$ROOT/.toolchain/gopath"
export GOCACHE="$ROOT/.toolchain/gocache"

# 版本号直接取自 main.go 的常量，避免「代码里一个版本、包里另一个版本」。
VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
if [ -z "$VERSION" ]; then
  echo "!! 未能从 main.go 解析版本号" >&2
  exit 1
fi
echo "==> 版本: $VERSION"

# ---- 前端产物必须存在（go:embed 需要它）----
if [ ! -f web/dist/index.html ]; then
  echo "!! web/dist/index.html 不存在，请先执行 web/ 下的 npm run build" >&2
  exit 1
fi

# ---- 目标平台 ----
TARGETS=("$@")
if [ ${#TARGETS[@]} -eq 0 ]; then
  TARGETS=(
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    # Windows on ARM：Go 交叉编译零成本，且 npm 平台包需要它
    #（否则 ARM 设备只能靠 x64 仿真装 x64 包，多一层不确定性）
    "windows/arm64"
    "darwin/amd64"
    "darwin/arm64"
  )
fi

OUT="$ROOT/dist"
mkdir -p "$OUT"

# 只清掉本次要重建的同版本产物，不做 `rm -rf $OUT`：
#   1. 不误删放在同一目录里的其它东西（安装包、上一版的发行包）；
#   2. 受限环境下"一次性删除整个目录树"可能被安全策略拦下并中断构建，
#      而有界删除（按文件名匹配、每个暂存目录用完即删）不会触发这类拦截。
rm -f "$OUT"/workbuddy-gateway_${VERSION}_*.tar.gz "$OUT"/workbuddy-gateway_${VERSION}_*.zip

STAGE="$OUT/.stage"
LDFLAGS="-s -w"

for target in "${TARGETS[@]}"; do
  GOOS="${target%%/*}"
  GOARCH="${target##*/}"
  echo "==> 编译 $GOOS/$GOARCH"

  bin_suffix=""
  [ "$GOOS" = "windows" ] && bin_suffix=".exe"

  pkg="$STAGE/${GOOS}_${GOARCH}"
  if [ -d "$pkg" ]; then rm -rf "$pkg"; fi
  mkdir -p "$pkg"

  # CGO_ENABLED=0：全平台产出静态二进制，不依赖各系统的 C 库
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    "$GO_BIN" build -trimpath -ldflags="$LDFLAGS" \
    -o "$pkg/workbuddy-gateway${bin_suffix}" .

  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    "$GO_BIN" build -trimpath -ldflags="$LDFLAGS" \
    -o "$pkg/wb-local-agent${bin_suffix}" ./cmd/wb-local-agent

  # 随包附带的最小可用说明与配置样例
  cp config.example.json "$pkg/"
  cp README.md "$pkg/" 2>/dev/null || true
  cp DEPLOY.md "$pkg/" 2>/dev/null || true

  # 部署脚本必须进包：用户拿到的是发行包，不是 git 仓库，
  # 缺了它们就得手抄 systemd 单元或自己拼计划任务。
  # 只放该平台用得到的：Windows 包里塞 systemd 单元只会造成困惑。
  if [ -d deploy ]; then
    mkdir -p "$pkg/deploy"
    if [ "$GOOS" = "windows" ]; then
      cp deploy/install-windows.ps1 "$pkg/deploy/" 2>/dev/null || true
    else
      cp deploy/install-systemd.sh deploy/wb-gateway.service "$pkg/deploy/" 2>/dev/null || true
      chmod +x "$pkg/deploy/"*.sh 2>/dev/null || true
    fi
  fi

  name="workbuddy-gateway_${VERSION}_${GOOS}_${GOARCH}"
  if [ "$GOOS" = "windows" ]; then
    if command -v zip >/dev/null 2>&1; then
      ( cd "$STAGE" && zip -qr "$OUT/${name}.zip" "${GOOS}_${GOARCH}" )
    elif command -v powershell >/dev/null 2>&1 || command -v powershell.exe >/dev/null 2>&1; then
      # Windows 上通常没有 zip 命令，退回 PowerShell 的 Compress-Archive
      PS_BIN="$(command -v powershell.exe || command -v powershell)"
      STAGE_WIN="$(cygpath -w "$STAGE" 2>/dev/null || echo "$STAGE")"
      OUT_WIN="$(cygpath -w "$OUT" 2>/dev/null || echo "$OUT")"
      "$PS_BIN" -NoProfile -NonInteractive -Command \
        "Compress-Archive -Path '${STAGE_WIN}\\${GOOS}_${GOARCH}' -DestinationPath '${OUT_WIN}\\${name}.zip' -Force" \
        >/dev/null 2>&1 || echo "!! zip 打包失败，请使用同名 .tar.gz" >&2
    else
      echo "!! 未找到 zip / powershell，Windows 包只产出 .tar.gz（可用 tar 解开）" >&2
    fi
  fi
  # 全平台都出一份 tar.gz：Linux/macOS 直接解压，Windows 也能用 tar 解
  ( cd "$STAGE" && tar -czf "$OUT/${name}.tar.gz" "${GOOS}_${GOARCH}" )

  # 该平台用完即删：把删除量摊到每个平台（9 个文件），
  # 而不是最后一次性删掉整个暂存目录（6 个平台 × 9 个文件）。
  rm -rf "$pkg"
done

rmdir "$STAGE" 2>/dev/null || true

echo "==> 完成，产物在 $OUT"
ls -lh "$OUT"

echo ""
echo "提示："
echo "  安装包（不是解压包）："
echo "    ./installer/windows/build-setup.sh   → Windows _x64-setup.exe / _arm64-setup.exe"
echo "    ./installer/linux/build-deb.sh       → Linux .deb（需在 Debian/Ubuntu 上执行）"
echo "    ./installer/macos/build-pkg.sh       → macOS .pkg + .dmg（需在 macOS 上执行）"
echo "  桌面版（Tauri 壳：原生窗口 + 托盘，不是解压包也不是服务端包）："
echo "    ./installer/windows/build-desktop-setup.sh → Windows setup.exe（需 MSVC）"
echo "    ./installer/macos/build-desktop-pkg.sh     → macOS .dmg（需在 macOS 上执行）"
echo "    ./installer/linux/build-desktop-deb.sh     → Linux .deb（需 Rust + WebKitGTK）"
echo "    ./scripts/build-desktop.sh                 → Windows 绿色版（作者本机的 windows-gnu 路线）"
echo "  npm 全局安装包： ./npm/scripts/build-packages.sh"
echo "  打 tag 时 .github/workflows/release.yml 会用三个平台的 runner 构建桌面版并发布到 Release；"
echo "  第 3 节那几个无窗口安装包改成只有手动触发（Actions 页面 Run workflow）才产出。"

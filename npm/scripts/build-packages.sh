#!/usr/bin/env bash
# 把 scripts/release.sh 产出的发行包填充进 npm 平台包，并打成可发布的 .tgz。
#
# 用法：
#   ./scripts/release.sh                 # 先产出 dist/*.tar.gz
#   ./npm/scripts/build-packages.sh      # 再填充并打包 npm 包（本脚本）
#
# 为什么不直接在平台包里编译：编译只做一次（release.sh），npm 包只是"换个壳装载"。
# 两边各自编译会出现"release 包里是 A 版本、npm 包里是 B 版本"这种极难发现的问题。
#
# 产物：npm/dist/*.tgz（PACK_ONLY=1 时只填不打包，便于本地检查）
set -euo pipefail

NPM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$NPM_DIR/.." && pwd)"
cd "$ROOT"

VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
if [ -z "$VERSION" ]; then
  echo "!! 未能从 main.go 解析版本号" >&2
  exit 1
fi
echo "==> 版本: $VERSION"

if [ ! -d "$ROOT/dist" ] || [ -z "$(ls -A "$ROOT/dist" 2>/dev/null)" ]; then
  echo "!! 未找到发行包，请先执行 ./scripts/release.sh" >&2
  exit 1
fi

# node/npm：与 scripts/build.sh 用同一套定位逻辑
NODE_BIN="${NODE_BIN:-}"
if [ -z "$NODE_BIN" ]; then
  for cand in \
    "C:/Users/$USERNAME/.workbuddy/binaries/node/versions/22.22.2-3" \
    "$(dirname "$(command -v node 2>/dev/null || true)")"; do
    if [ -n "$cand" ] && [ -x "$cand/node.exe" ]; then NODE_BIN="$cand"; break; fi
    if [ -n "$cand" ] && [ -x "$cand/node" ]; then NODE_BIN="$cand"; break; fi
  done
fi
NPM_CMD="$NODE_BIN/npm"
[ -x "$NPM_CMD" ] || NPM_CMD="npm"

# 发行包目录名 → npm 平台键
# 键必须与 npm/scripts/platform.js 的 PLATFORMS 完全一致
PLATFORM_MAP=(
  "windows_amd64:win32-x64"
  "windows_arm64:win32-arm64"
  "darwin_amd64:darwin-x64"
  "darwin_arm64:darwin-arm64"
  "linux_amd64:linux-x64"
  "linux_arm64:linux-arm64"
)

echo "==> 同步版本号到各 package.json"
python - "$VERSION" <<'PY'
import io, json, sys, glob, os
version = sys.argv[1]
files = ["npm/package.json"] + glob.glob("npm/platform/*/package.json")
for path in files:
    doc = json.load(io.open(path, encoding="utf-8"))
    doc["version"] = version
    # 主包的 optionalDependencies 必须与平台包版本严格一致，
    # 否则 npm 会去装一个不存在的版本号而静默失败（安装看起来成功、跑起来没有二进制）。
    if "optionalDependencies" in doc:
        doc["optionalDependencies"] = {k: version for k in doc["optionalDependencies"]}
    io.open(path, "w", encoding="utf-8").write(json.dumps(doc, ensure_ascii=False, indent=2) + "\n")
print("  updated", len(files), "package.json")
PY

TMP="$ROOT/npm/.tmp-extract"
mkdir -p "$TMP"

# 本轮真正填充过的平台包。打包阶段只打这些：
# 平台包目录里残留着**上一轮**的二进制，如果无脑 npm pack，
# 「只构建了 linux/amd64」会顺手把 darwin/windows 的旧二进制也发出去 ——
# 版本号相同、内容却是旧代码，属于最难发现的一类错误产物。
FILLED=()

echo "==> 填充平台包二进制"
for entry in "${PLATFORM_MAP[@]}"; do
  release_suffix="${entry%%:*}"
  pkg_suffix="${entry##*:}"
  tarball="$ROOT/dist/workbuddy-gateway_${VERSION}_${release_suffix}.tar.gz"
  pkg_dir="$NPM_DIR/platform/workbuddy-gateway-${pkg_suffix}"

  if [ ! -f "$tarball" ]; then
    echo "  - $pkg_suffix: 跳过（无 $release_suffix 发行包）"
    continue
  fi
  if [ ! -d "$pkg_dir" ]; then
    echo "  - $pkg_suffix: 跳过（无对应 npm 平台包目录）"
    continue
  fi

  rm -rf "$TMP/$release_suffix"
  mkdir -p "$TMP/$release_suffix"
  # 解压带重试：Windows 上 dist/ 里的产物是几秒前刚写出来的大文件（每个 6-7MB），
  # 杀软/索引器可能正持有句柄，tar 会随机失败一次。失败后立即重试基本都能过；
  # 真正持续失败（包损坏等）仍会退出，不会被这次重试掩盖。
  ok=0
  for attempt in 1 2 3; do
    if tar -xzf "$tarball" -C "$TMP/$release_suffix" 2>/tmp/wbg-tar-err; then
      ok=1
      break
    fi
    echo "  ! $pkg_suffix: 解压失败（第 $attempt 次）：$(head -1 /tmp/wbg-tar-err 2>/dev/null)" >&2
    rm -rf "$TMP/$release_suffix"
    mkdir -p "$TMP/$release_suffix"
    sleep 1
  done
  if [ "$ok" != "1" ]; then
    echo "  !! $pkg_suffix: 连续 3 次解压 $tarball 失败，请检查该发行包" >&2
    exit 1
  fi

  # 发行包内固定是一层同名目录（release.sh 就是这么打的），直接拼路径。
  # 不要用 `find -maxdepth 2 -type d | tail -1` 之类的启发式：包里还有 deploy/ 子目录，
  # 取到它就会**静默复制 0 个文件**，最后产出一个"看起来成功但没有二进制"的空包。
  src_dir="$TMP/$release_suffix/$release_suffix"
  if [ ! -d "$src_dir" ]; then
    echo "  !! $pkg_suffix: 发行包结构与预期不符（缺 $release_suffix/ 目录）" >&2
    exit 1
  fi

  # 只删目录里的文件，不删目录：删除量有界，也避免瞬时大批量删除被安全策略拦下
  if [ -d "$pkg_dir/bin" ]; then rm -f "$pkg_dir/bin"/*; fi
  mkdir -p "$pkg_dir/bin"
  # 两个二进制都要放进同一个 bin/：网关按「与自己同目录」查找本机代理。
  # 用显式 if 而不是 `[ -f ] && cp`：后者在 set -e 下的行为依赖 bash 对
  # AND 列表的豁免规则，可读性差且容易在改动后突然变成"静默跳过"。
  for f in workbuddy-gateway workbuddy-gateway.exe wb-local-agent wb-local-agent.exe; do
    if [ -f "$src_dir/$f" ]; then
      cp "$src_dir/$f" "$pkg_dir/bin/"
      chmod +x "$pkg_dir/bin/$f"
    fi
  done

  # 网关二进制是硬要求：缺了它这个平台包就是废的，必须当场失败而不是继续打包。
  gw_name="workbuddy-gateway"
  case "$release_suffix" in
    windows_*) gw_name="workbuddy-gateway.exe" ;;
  esac
  if [ ! -f "$pkg_dir/bin/$gw_name" ]; then
    echo "  !! $pkg_suffix: 未复制到 $gw_name（源目录 $src_dir 内容如下）" >&2
    ls -1 "$src_dir" >&2
    exit 1
  fi

  count="$(ls -1 "$pkg_dir/bin" | wc -l | tr -d ' ')"
  echo "  + $pkg_suffix: $count 个二进制（$(du -sh "$pkg_dir/bin" | cut -f1)）"
  FILLED+=("$pkg_suffix")

  # 该平台的解压暂存用完即删，把删除量摊开
  rm -rf "$TMP/$release_suffix"
done

rmdir "$TMP" 2>/dev/null || true

if [ ${#FILLED[@]} -eq 0 ]; then
  echo "!! 没有任何平台包被填充（dist/ 下缺少本版本的发行包？）" >&2
  exit 1
fi

if [ "${PACK_ONLY:-0}" = "1" ]; then
  echo "==> PACK_ONLY=1，跳过打包"
  exit 0
fi

OUT="$NPM_DIR/dist"
mkdir -p "$OUT"
# 只清 tgz，不删目录（同上：有界删除）
rm -f "$OUT"/*.tgz

echo "==> npm pack（只打本轮填充过的平台包）"
for pkg_suffix in "${FILLED[@]}"; do
  pkg="$NPM_DIR/platform/workbuddy-gateway-${pkg_suffix}"
  ( cd "$pkg" && "$NPM_CMD" pack --pack-destination "$OUT" >/dev/null )
  echo "  + workbuddy-gateway-${pkg_suffix}"
done

# 未被填充的平台包提醒一句：不然「为什么 Release 里少了 darwin 包」会很难查
for entry in "${PLATFORM_MAP[@]}"; do
  pkg_suffix="${entry##*:}"
  found=0
  for f in "${FILLED[@]}"; do
    [ "$f" = "$pkg_suffix" ] && found=1 && break
  done
  if [ "$found" = "0" ]; then
    echo "  - workbuddy-gateway-${pkg_suffix} 未打包（本轮未填充；如需它请先构建对应发行包）"
  fi
done

( cd "$NPM_DIR" && "$NPM_CMD" pack --pack-destination "$OUT" >/dev/null )
echo "  + workbuddy-gateway（主包）"

echo "==> 完成，npm 包在 $OUT"
ls -lh "$OUT"

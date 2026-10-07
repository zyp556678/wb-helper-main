# -*- coding: utf-8 -*-
"""把桌面构建产物打包进 dist/：便携包 + 安装包同步。

## 为什么需要这个脚本

`tauri build` 的产物落在 `.toolchain/cargo-target/release/bundle/nsis/`，
而用户实际拿的是 `dist/` 下的安装包 —— 两者之间没有任何自动同步。
**漏跑这一步的后果是：改完代码、构建成功、dist 里还是旧包**，
装上去现象一点没变，很容易误判成「修复没生效」。（真实踩过。）

所以构建桌面版请用 `scripts/build-desktop.sh`，它把 tauri build 与本脚本
串成一条命令，不给「忘记同步」留机会。

用法：
    python scripts/package-desktop.py            # 打包并同步
    python scripts/package-desktop.py --check     # 只检查 dist 是否落后于构建产物
"""
import argparse
import hashlib
import json
import os
import shutil
import sys
import time
import zipfile

# 仓库根目录按脚本位置推导，不写死绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")
STAGE = os.path.join(ROOT, ".toolchain", "portable-stage")
RELEASE = os.path.join(ROOT, ".toolchain", "cargo-target", "release")


def tauri_field(name: str) -> str:
    """从 desktop/src-tauri/tauri.conf.json 读一个字段。

    产品名与版本号**只从那里读**，不在这里写死：`tauri build` 的产物文件名就是
    `<productName>_<version>_x64-setup.exe`，而 dist/ 里的目标名也要跟着同一个版本走。
    任何一处写死，升版本时都会变成「构建成功、这个脚本却去找一个不存在的文件」——
    正好是本文档开头说的那类最难查的误判。
    """
    path = os.path.join(ROOT, "desktop", "src-tauri", "tauri.conf.json")
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)[name]


VERSION = tauri_field("version")
PRODUCT = tauri_field("productName")

SETUP_SRC = os.path.join(RELEASE, "bundle", "nsis", f"{PRODUCT}_{VERSION}_x64-setup.exe")
SETUP_DST = os.path.join(DIST, f"workbuddy-gateway-desktop_{VERSION}_x64-setup.exe")
ZIP_DST = os.path.join(DIST, f"workbuddy-gateway-desktop_{VERSION}_x64-portable.zip")

# 便携包内的文件：源路径 → 包内相对路径
MEMBERS = [
    (os.path.join(ROOT, "workbuddy-gateway.exe"), "bin/workbuddy-gateway.exe"),
    (os.path.join(ROOT, "wb-local-agent.exe"), "bin/wb-local-agent.exe"),
    (os.path.join(ROOT, "desktop", "src-tauri", "WebView2Loader.dll"), "WebView2Loader.dll"),
    (os.path.join(RELEASE, "workbuddy-gateway-desktop.exe"), "workbuddy-gateway-desktop.exe"),
]


def stamp(path: str) -> str:
    """把文件的修改时间与大小拼成一行可读标识，用于判断新旧。"""
    if not os.path.exists(path):
        return "(不存在)"
    return f"{time.strftime('%H:%M:%S', time.localtime(os.path.getmtime(path)))} {os.path.getsize(path)}B"


def sha16(path: str) -> str:
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()[:16]


def check() -> int:
    """dist 里的安装包是否落后于刚构建的产物。落后就返回非零。"""
    if not os.path.exists(SETUP_SRC):
        print(f"✗ 找不到构建产物：{SETUP_SRC}\n  先跑 scripts/build-desktop.sh")
        return 1
    if not os.path.exists(SETUP_DST):
        print(f"✗ dist 里还没有安装包：{SETUP_DST}")
        return 1
    stale = os.path.getmtime(SETUP_SRC) > os.path.getmtime(SETUP_DST) + 1
    print(f"  构建产物 {stamp(SETUP_SRC)}")
    print(f"  dist     {stamp(SETUP_DST)}")
    if stale:
        print("✗ dist 落后于构建产物 —— 用户装到的会是旧包。跑 scripts/build-desktop.sh 同步。")
        return 1
    print("✓ dist 与构建产物一致")
    return 0


def package() -> int:
    for src, _ in MEMBERS:
        if not os.path.exists(src):
            print(f"✗ 缺少打包源文件：{src}")
            return 1
    if not os.path.exists(SETUP_SRC):
        print(f"✗ 找不到安装包产物：{SETUP_SRC}\n  先跑 scripts/build-desktop.sh")
        return 1

    # 使用说明沿用上一版：包内结构不变，改的只是二进制。
    if os.path.exists(ZIP_DST):
        with zipfile.ZipFile(ZIP_DST) as existing:
            note = existing.read("使用说明.txt")
    else:
        note = "WorkBuddy 网关桌面版\n\n双击 workbuddy-gateway-desktop.exe 启动。\n".encode("utf-8")

    if os.path.isdir(STAGE):
        shutil.rmtree(STAGE)
    os.makedirs(os.path.join(STAGE, "bin"))

    for src, rel in MEMBERS:
        dst = os.path.join(STAGE, rel.replace("/", os.sep))
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        shutil.copy2(src, dst)
    with open(os.path.join(STAGE, "使用说明.txt"), "wb") as handle:
        handle.write(note)

    if os.path.exists(ZIP_DST):
        os.remove(ZIP_DST)
    with zipfile.ZipFile(ZIP_DST, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for _, rel in MEMBERS + [(None, "使用说明.txt")]:
            zf.write(os.path.join(STAGE, rel.replace("/", os.sep)), rel)

    shutil.copy2(SETUP_SRC, SETUP_DST)

    # 同步完立刻自检：这一步如果没生效，用户装到的就是旧包，
    # 而现象是「改了代码却没变化」——最难查的那类问题。
    print(f"  安装包   {stamp(SETUP_DST)}  sha256:{sha16(SETUP_DST)[:16]}")
    print(f"  便携包   {stamp(ZIP_DST)}  sha256:{sha16(ZIP_DST)[:16]}")
    print(f"  网关     {stamp(os.path.join(ROOT, 'workbuddy-gateway.exe'))}")
    return check()


def main() -> int:
    parser = argparse.ArgumentParser(description="打包桌面产物到 dist/")
    parser.add_argument("--check", action="store_true", help="只检查 dist 是否落后，不打包")
    args = parser.parse_args()
    return check() if args.check else package()


if __name__ == "__main__":
    sys.exit(main())

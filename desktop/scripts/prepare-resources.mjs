// 把 Go 侧的二进制复制进壳的资源目录，供 `tauri build` 打进安装包。
//
// 设计要点：
//  1. **缺文件就失败**，不做"尽力而为"。一个装上去没有后端的安装包，
//     用户看到的是"启动失败：未找到 workbuddy-gateway"，而这本该在构建期就被拦下。
//  2. 复制后校验大小，防止半截文件被当成有效产物打进去。
//  3. 打印版本号，便于确认打进包里的到底是哪一版 Go 后端。

import { copyFileSync, existsSync, mkdirSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";

const here = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(here, "..", "..");
const resourceDir = resolve(here, "..", "src-tauri", "bin");

const BINARIES = ["workbuddy-gateway.exe", "wb-local-agent.exe"];

/**
 * 从 cargo 构建产物里找回 WebView2Loader.dll。
 *
 * 它由 webview2-com-sys 的 build script 解出来，所以新克隆的仓库跑一次
 * `cargo build` 就有了 —— 不必把微软的二进制提交进仓库。
 * 顺序上优先 release + x64（打包用的就是这套）。
 *
 * @returns 命中的源路径；都没找到返回 null。
 */
function recoverWebViewLoader(root, target) {
  const targetDir = join(root, ".toolchain", "cargo-target");
  const candidates = [
    join(targetDir, "release", "WebView2Loader.dll"),
    join(targetDir, "debug", "WebView2Loader.dll"),
  ];
  // build script 的原始输出目录（名字带哈希后缀，只能扫）
  try {
    const buildDir = join(targetDir, "release", "build");
    if (existsSync(buildDir)) {
      for (const entry of readdirSync(buildDir)) {
        if (entry.startsWith("webview2-com-sys-")) {
          candidates.push(join(buildDir, entry, "out", "x64", "WebView2Loader.dll"));
        }
      }
    }
  } catch {
    // 目录不可读就跳过：候选为空时下面自然返回 null。
  }

  for (const candidate of candidates) {
    if (existsSync(candidate)) {
      mkdirSync(dirname(target), { recursive: true });
      copyFileSync(candidate, target);
      return candidate;
    }
  }
  return null;
}

mkdirSync(resourceDir, { recursive: true });

let failed = false;
for (const name of BINARIES) {
  const source = join(projectRoot, name);
  if (!existsSync(source)) {
    console.error(`[prepare] 缺少 ${name}，请先在项目根目录执行 Go 构建`);
    failed = true;
    continue;
  }
  const size = statSync(source).size;
  if (size < 1024 * 1024) {
    console.error(`[prepare] ${name} 只有 ${size} 字节，疑似损坏或半截产物`);
    failed = true;
    continue;
  }
  const target = join(resourceDir, name);
  copyFileSync(source, target);
  console.log(`[prepare] ${name} → ${target} (${(size / 1024 / 1024).toFixed(1)} MB)`);
}

if (failed) process.exit(1);

// WebView2Loader.dll 必须与壳同目录，否则壳启动后会静默退出（进程存活但没有窗口、
// 也不拉起后端，release 版无控制台所以看不到任何错误）。
//
// 为什么它是必需资源：Tauri 在 x86_64-pc-windows-gnu 目标下**动态链接** WebView2Loader，
// 但 tauri-bundler 不会自动把它打进安装包（它默认假设 MSVC 的静态链接路径），
// 于是安装出来的应用一启动就挂。这里作为固定资源随包分发，
// 并在 tauri.conf.json 的 bundle.resources 中显式声明。
//
// 它**不入库**（是微软的第三方二进制，不是本项目代码）。缺了就按下面的顺序找回：
// 1) 已放在 src-tauri/ 下的（手动放的最优先，允许覆盖版本）；
// 2) cargo 构建产物里的 —— webview2-com-sys 的 build script 会把它解到
//    target/<profile>/ 与 target/<profile>/build/webview2-com-sys-*/out/x64/ 下。
// 这样新克隆的仓库跑一次构建就能自愈，不必先把二进制提交进仓库。
const webviewLoader = resolve(here, "..", "src-tauri", "WebView2Loader.dll");
if (!existsSync(webviewLoader)) {
  const recovered = recoverWebViewLoader(projectRoot, webviewLoader);
  if (!recovered) {
    console.error(
      `[prepare] 缺少 WebView2Loader.dll，且在构建产物里也没找到：${webviewLoader}\n` +
        "          先跑一次 `cargo build`（webview2-com-sys 会解出这个 DLL），" +
        "或手动从 .toolchain/cargo-target/<profile>/ 复制过来。",
    );
    process.exit(1);
  }
  console.log(`[prepare] WebView2Loader.dll 从构建产物找回：${recovered}`);
}
console.log(
  `[prepare] WebView2Loader.dll 就位 (${(statSync(webviewLoader).size / 1024).toFixed(0)} KB)`,
);

// 版本号取自治的网关二进制本身，而不是另抄一份常量 —— 避免文档/包名与真实后端不一致。
try {
  const version = execFileSync(join(resourceDir, BINARIES[0]), ["version"], {
    encoding: "utf8",
    timeout: 20000,
  }).trim();
  console.log(`[prepare] 已打包的网关版本：${version}`);
} catch (error) {
  console.warn(`[prepare] 未能读取网关版本（不影响构建）：${error.message}`);
}

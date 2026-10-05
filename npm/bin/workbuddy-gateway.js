#!/usr/bin/env node
// WorkBuddy 网关的 npm 入口：把参数转发给网关二进制。
//
// 体验设计（对齐 switch 的 npm 版：一条命令起服务并打开浏览器）：
//   1. 不带任何参数 → 起服务并自动打开面板。这是绝大多数人想要的默认行为。
//   2. 显式 `serve` → 起服务，但**不**自动开浏览器（在服务器 / Docker 里跑时不需要）。
//   3. 只给参数（如 `--port 9000`）→ 等价于 `serve --port 9000`。
//   4. `status` / `version` / `help` → 原样透传。
//
// 工作目录固定为数据目录（默认 ~/.wb-gateway，可用 WB_GATEWAY_DATA_DIR 覆盖）：
// 全局安装后用户会在任意目录执行命令，若拿当前目录当数据目录，
// 凭据会被写到"你当时恰好 cd 到哪儿"，换个目录启动就发现账号池空了。

const { spawn, spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const { platformKey, PLATFORMS, dataDir } = require("../scripts/platform");

const KEY = platformKey();
const NAMES = PLATFORMS[KEY] || { gateway: "workbuddy-gateway", agent: "wb-local-agent" };
// 本脚本就在 bin/ 目录里，postinstall 把二进制也复制到同一个目录
// （`path.join(__dirname, "..")` 会指到包根，那里没有二进制，已踩过一次）。
const BIN_DIR = __dirname;
const GATEWAY = process.env.WB_GATEWAY_BINARY
  ? path.resolve(process.env.WB_GATEWAY_BINARY)
  : path.join(BIN_DIR, NAMES.gateway);

function ensureBinary() {
  if (fs.existsSync(GATEWAY)) return true;
  // 可能是 `npm ci --omit=optional` 或手动删过 bin/，重跑一次安装脚本而不是直接报错
  try {
    require("../scripts/install.js");
  } catch {
    /* install.js 自己会打印原因 */
  }
  return fs.existsSync(GATEWAY);
}

function fail(msg) {
  console.error(`workbuddy-gateway: ${msg}`);
  process.exit(1);
}

/** 打开浏览器；失败不影响主进程（无桌面环境时很常见，不该让服务起不来）。 */
function openBrowser(url) {
  const cmd =
    process.platform === "win32"
      ? ["cmd", ["/c", "start", "", url]]
      : process.platform === "darwin"
        ? ["open", [url]]
        : ["xdg-open", [url]];
  try {
    spawn(cmd[0], cmd[1], { stdio: "ignore", detached: true }).unref();
  } catch {
    /* 忽略：用户可手动访问面板地址 */
  }
}

function main() {
  if (!ensureBinary()) {
    fail(
      `未找到网关二进制（${GATEWAY}）。\n` +
        `  可尝试：npm rebuild -g workbuddy-gateway\n` +
        `  或指定本地二进制：WB_GATEWAY_BINARY=/path/to/workbuddy-gateway`,
    );
  }

  const argv = process.argv.slice(2);
  const hasSubcommand = argv.length > 0 && !argv[0].startsWith("-");
  const subcommand = hasSubcommand ? argv[0] : "";
  const serveFlags = hasSubcommand ? argv.slice(1) : argv;

  // 只给参数时补上 serve
  let args;
  let autoOpen = false;
  if (!hasSubcommand && argv.length === 0) {
    args = ["serve"];
    autoOpen = true;
  } else if (!hasSubcommand) {
    args = ["serve", ...serveFlags];
    autoOpen = true;
  } else {
    args = argv;
  }

  // 数据目录：首次运行自动创建（凭据、config.json、缓存、统计都在这里）
  const dir = dataDir();
  try {
    fs.mkdirSync(dir, { recursive: true });
  } catch (err) {
    fail(`无法创建数据目录 ${dir}: ${err.message}`);
  }

  if (autoOpen) {
    const port = portFromArgs(serveFlags) || 8317;
    // 网关启动时会先同步拉一次模型目录再监听端口，所以面板要等一会儿才可用。
    // 这里只做"打开浏览器"这一件事，不做健康探测——省得为了几秒等待引入轮询逻辑。
    setTimeout(() => openBrowser(`http://127.0.0.1:${port}/panel/`), 3000);
    console.log(`workbuddy-gateway: 数据目录 ${dir}`);
  }

  // stdio 继承：日志直接打到终端，Ctrl+C 也能传到子进程
  const result = spawnSync(GATEWAY, args, { cwd: dir, stdio: "inherit" });
  if (result.error) fail(`启动失败: ${result.error.message}`);
  process.exit(result.status ?? 1);
}

/** 从参数里取 --port / -port 的值（仅用于拼面板地址）。 */
function portFromArgs(argv) {
  for (let i = 0; i < argv.length - 1; i++) {
    const a = argv[i];
    if (a === "--port" || a === "-port" || a === "-p") return argv[i + 1];
    const m = /^(?:--|-)?port=(.+)$/.exec(a);
    if (m) return m[1];
  }
  return "";
}

main();

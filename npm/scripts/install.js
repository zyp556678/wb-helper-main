// postinstall：把「平台包」里的二进制复制到本包的 bin/ 目录。
//
// 为什么用「主包 + 平台包（optionalDependencies）」而不是安装时联网下载：
//   1. 二进制随平台包一起进 npm registry，国内镜像（npmmirror）也能装，
//      不依赖 GitHub Releases 可达；
//   2. 校验交给 npm 自身的完整性校验，不用我们再实现一套校验逻辑。
//
// 环境变量覆盖：
//   WB_GATEWAY_BINARY=<本地网关二进制路径>   离线 / 本地开发（直接复制，不查平台包）
//   WB_LOCAL_AGENT_BINARY=<本地代理路径>     同理；缺省时从同目录推断

const fs = require("fs");
const path = require("path");

const { platformKey, platformPackageName, PLATFORMS } = require("./platform");

const key = platformKey();
const binDir = path.join(__dirname, "..", "bin");

function fail(msg) {
  console.error(`workbuddy-gateway install: ${msg}`);
  console.error(
    "安装失败。请确认平台的 npm 包已装好（npm 会按 os/cpu 自动选择），" +
      "或设置 WB_GATEWAY_BINARY 指向本地二进制后重试。",
  );
  process.exit(1);
}

/** 复制单个二进制，并做最小合理性校验（防止把占位文件当成真二进制装上）。 */
function copyBinary(src, destName) {
  if (!fs.existsSync(src)) return false;
  fs.mkdirSync(binDir, { recursive: true });
  const dest = path.join(binDir, destName);
  fs.copyFileSync(src, dest);
  if (process.platform !== "win32") fs.chmodSync(dest, 0o755);

  const size = fs.statSync(dest).size;
  // 网关含内嵌面板，实际约 9MB；小于 1MB 基本都是错误产物或 LFS 占位文件
  const minSize = destName.includes("local-agent") ? 512 * 1024 : 1024 * 1024;
  if (size < minSize) {
    fs.unlinkSync(dest);
    fail(`二进制异常（${path.basename(src)} 仅 ${size} 字节）`);
  }
  console.log(`workbuddy-gateway: ${destName} (${(size / 1048576).toFixed(1)}MB)`);
  return true;
}

function main() {
  // 1) 本地二进制覆盖（离线安装 / 本地开发）
  if (process.env.WB_GATEWAY_BINARY) {
    const gateway = path.resolve(process.env.WB_GATEWAY_BINARY);
    if (!fs.existsSync(gateway)) fail(`WB_GATEWAY_BINARY 指向的文件不存在: ${gateway}`);
    if (!copyBinary(gateway, path.basename(gateway))) fail(`复制失败: ${gateway}`);

    const agent =
      process.env.WB_LOCAL_AGENT_BINARY ||
      path.join(path.dirname(gateway), PLATFORMS[key]?.agent || "wb-local-agent");
    if (!copyBinary(agent, PLATFORMS[key]?.agent || "wb-local-agent")) {
      // 本机代理是可选的：缺它只是面板少一个页面，不该让整个安装失败
      console.log("workbuddy-gateway: 未找到本机代理，跳过（面板的「本机客户端」页会自动隐藏）");
    }
    return;
  }

  if (!key) {
    console.warn(
      `workbuddy-gateway: 跳过平台 ${process.platform}-${process.arch}（暂未提供预编译二进制）。\n` +
        "可自行交叉编译后设置 WB_GATEWAY_BINARY 安装。",
    );
    process.exit(0);
  }

  // 2) 从平台包复制
  let pkgDir;
  try {
    pkgDir = path.dirname(require.resolve(`${platformPackageName(key)}/package.json`));
  } catch {
    fail(
      `未找到平台包 ${platformPackageName(key)}。` +
        "常见原因：用 --no-optional / --omit=optional 安装，或该平台的包未被发布。",
    );
    return;
  }

  const names = PLATFORMS[key];
  if (!copyBinary(path.join(pkgDir, "bin", names.gateway), names.gateway)) {
    fail(`平台包目录里缺少 ${names.gateway}: ${pkgDir}`);
  }
  if (!copyBinary(path.join(pkgDir, "bin", names.agent), names.agent)) {
    console.log("workbuddy-gateway: 平台包里没有本机代理，跳过（面板的「本机客户端」页会自动隐藏）");
  }
}

main();

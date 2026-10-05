// 平台映射与运行目录解析：install.js 与 bin 入口共用同一份，避免两边"各写一套"后悄悄漂移。
//
// 命名约定必须与 npm/platform/*/package.json 的 name 以及 scripts/release.sh 的
// GOOS/GOARCH 目标一一对应；任何一处改了，另外两处都跟着改。

const os = require("os");
const path = require("path");

/** 平台键（`${platform}-${arch}`）→ 该平台下两个二进制的文件名。 */
const PLATFORMS = {
  "win32-x64": { gateway: "workbuddy-gateway.exe", agent: "wb-local-agent.exe" },
  "win32-arm64": { gateway: "workbuddy-gateway.exe", agent: "wb-local-agent.exe" },
  "darwin-x64": { gateway: "workbuddy-gateway", agent: "wb-local-agent" },
  "darwin-arm64": { gateway: "workbuddy-gateway", agent: "wb-local-agent" },
  "linux-x64": { gateway: "workbuddy-gateway", agent: "wb-local-agent" },
  "linux-arm64": { gateway: "workbuddy-gateway", agent: "wb-local-agent" },
};

/** 当前平台键；不支持的平台返回空串。 */
function platformKey(platform = process.platform, arch = process.arch) {
  return PLATFORMS[`${platform}-${arch}`] ? `${platform}-${arch}` : "";
}

/** 平台包名。 */
function platformPackageName(key) {
  return `workbuddy-gateway-${key}`;
}

/**
 * 运行目录（工作目录）：凭据、config.json、模型缓存、统计都落在这里。
 *
 * 为什么固定而不是用当前目录：`npm i -g` 之后用户会在任意目录执行命令，
 * 若把工作目录当数据目录，凭据会被写到"你当时恰好 cd 到哪儿"，
 * 换个目录启动就发现账号池空了 —— 这是最难自查的一类问题。
 */
function dataDir(env = process.env) {
  if (env.WB_GATEWAY_DATA_DIR) return path.resolve(env.WB_GATEWAY_DATA_DIR);
  // Windows 用 %LOCALAPPDATA%\wb-gateway：与桌面壳、平台安装包落位一致，
  // 三个入口共用同一份账号池（README 的数据目录表也是这么写的）。
  // 其它平台沿用 ~/.wb-gateway。
  if (process.platform === "win32" && env.LOCALAPPDATA) {
    return path.join(env.LOCALAPPDATA, "wb-gateway");
  }
  return path.join(os.homedir(), ".wb-gateway");
}

module.exports = { PLATFORMS, platformKey, platformPackageName, dataDir };

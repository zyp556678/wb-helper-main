/**
 * 从 URL fragment 里接收桌面壳注入的接入密钥。
 *
 * ## 为什么要走 fragment
 *
 * 桌面壳给网关配了 `--api-key`（见 desktop/src-tauri/src/api_key.rs），
 * 于是面板第一次加载时 localStorage 里还没有密钥，所有接口都会 401。
 * 让用户手动去别处找密钥再粘贴，是没必要的摩擦 —— 壳自己就知道密钥。
 *
 * 选 fragment 而不是 query string，是因为 **fragment 不会发给服务器**：
 * 不会进网关的访问日志、也不会被反向代理记下来。query 会。
 *
 * 读出来之后立刻 `history.replaceState` 抹掉：既不在地址栏留着，
 * 也不留在浏览历史里（replaceState 是替换当前条目，不是新增）。
 *
 * ## 幂等
 *
 * 每次都覆盖写入，而不是"没有才写"：用户可能在面板里手动改过密钥，
 * 但壳每次启动都会重新注入它自己那份 —— 以壳为准才不会出现
 * 「面板显示一个、网关认另一个」的错位。
 */
export function consumeInjectedApiKey(): void {
  if (typeof window === "undefined") return;

  const hash = window.location.hash;
  if (!hash || !hash.includes("wb-key=")) return;

  let key = "";
  try {
    key = new URLSearchParams(hash.replace(/^#/, "")).get("wb-key") ?? "";
  } catch {
    // fragment 不是合法的查询串形态，当作没有注入处理。
    return;
  }

  if (key) {
    try {
      window.localStorage.setItem("workbuddy-gateway.api_key", key);
    } catch {
      // 存储不可用时只能作罢：后续接口会 401，面板会提示手动输入。
    }
  }

  // 无论有没有取到值，都要把 fragment 抹掉，避免它停留在地址栏里。
  const cleaned = window.location.pathname + window.location.search;
  window.history.replaceState(null, "", cleaned);
}

/**
 * 立即消费一次，并在之后每次 fragment 变化时再消费。
 *
 * 为什么需要监听 `hashchange`：壳在「重启网关」时，webview 已经停在
 * `/panel/` 上了，此时导航到 `/panel/#wb-key=...` 只是**同文档**改 fragment，
 * 页面不会重新加载，入口脚本也不会再跑一遍 —— 只听入口那一次的话，
 * 这条路径下密钥就注入不进去。
 */
export function watchInjectedApiKey(): void {
  consumeInjectedApiKey();
  window.addEventListener("hashchange", consumeInjectedApiKey);
}

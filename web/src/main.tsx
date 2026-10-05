import React from "react";
import ReactDOM from "react-dom/client";
import "@fontsource-variable/bricolage-grotesque";
import "@fontsource-variable/jetbrains-mono";

import App from "./App";
import "./index.css";
import { watchInjectedApiKey } from "./lib/injected-key";
import { applyTheme, getThemePreference, watchSystemTheme } from "./lib/theme";

// 必须在任何请求发出之前消费注入的密钥（App 挂载时就会拉接口）。
// 同时挂上 hashchange：壳重启时 webview 已停在面板上，那次导航只改 fragment、
// 不重新加载页面，入口脚本不会再跑。
watchInjectedApiKey();

// 挂载前先应用主题，避免深色模式下首屏闪白。
applyTheme(getThemePreference());
const stopWatchingSystemTheme = watchSystemTheme();
if (import.meta.hot) import.meta.hot.dispose(stopWatchingSystemTheme);

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);

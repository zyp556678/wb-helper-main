import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// 相对 base：产物会被 go:embed 进二进制，由服务端在任意路径下提供。
export default defineConfig({
  base: "./",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    strictPort: true,
    /* 显式绑定 IPv4 回环。默认值 `localhost` 由 DNS 解析决定，本机 Windows 上
       会解析成 `::1` → dev server 只监听 IPv6，`http://127.0.0.1:5173` 直接连不上
       （表现为 curl 返回 000、无头浏览器打开是空页、CDP 脚本点不到侧栏）。
       项目里其他服务（8317 / 8123 / 9223）都在 127.0.0.1，这里保持一致。 */
    host: "127.0.0.1",
    proxy: {
      "/panel/api": {
        /* 允许用 WB_DEV_API 指向别的网关实例。
           用途：本机 8317 常常已被**已安装的桌面版**占着，而开发时想验证的是
           刚编译出来的那份二进制 —— 直接改端口比互相抢端口干净。 */
        target: process.env.WB_DEV_API || "http://127.0.0.1:8317",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});

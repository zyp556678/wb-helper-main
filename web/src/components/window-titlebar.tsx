import * as React from "react";
import { Minus, Square, SquareStack, X } from "lucide-react";

import { cn } from "@/lib/utils";

/**
 * 桌面壳注入的窗口 API（`app.withGlobalTauri`）。浏览器里没有这个对象。
 *
 * 只声明用得到的几个方法，不引入 @tauri-apps/api：面板在浏览器里也要能跑，
 * 而那点功能不值得为它加一个只对桌面版有意义的依赖。
 */
interface ShellWindow {
  minimize(): Promise<void>;
  toggleMaximize(): Promise<void>;
  close(): Promise<void>;
  isMaximized(): Promise<boolean>;
}

function shellWindow(): ShellWindow | null {
  if (typeof window === "undefined") return null;
  const api = (window as unknown as { __TAURI__?: { window?: { getCurrentWindow?: () => ShellWindow } } })
    .__TAURI__;
  try {
    return api?.window?.getCurrentWindow?.() ?? null;
  } catch {
    return null;
  }
}

/**
 * 本页面是不是跑在**无系统标题栏**的桌面壳里。
 *
 * 判据是壳在地址上带的 `?host=desktop`（见 desktop/src-tauri 的 navigate_to_panel）：
 * 壳按平台决定要不要去掉系统标题栏 —— macOS 保留原生标签栏（无边框窗口在那边会丢掉
 * 边缘拖拽缩放与交通灯按钮，那是当地用户的基本预期），只有去掉时才带这个参数。
 *
 * 刻意不用 `window.__TAURI__` 是否存在来判断：那个对象在 macOS 上也有，
 * 区分不出「谁在给我画标题栏」。
 */
export function isFramelessShell(): boolean {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location.search).get("host") === "desktop";
}

/**
 * 自绘窗口标题栏：无边框窗口下唯一能拖动窗口、能最小化/最大化/关闭的地方。
 *
 * `data-tauri-drag-region` 由 Tauri 注入的脚本接管（按下即开始拖窗，双击最大化），
 * 所以**按钮上绝不能带这个属性** —— 带了就连点不动。
 *
 * 没有壳的窗口 API 时（有人在浏览器里手动带上 `?host=desktop`）按钮画成不可用，
 * 而不是整条不渲染：这样「界面上有没有这条顶栏」只由地址决定，排查时不会被
 * 「有没有壳」这第二层条件绕进去。
 */
export function WindowTitleBar() {
  const [visible] = React.useState(() => isFramelessShell());
  const [api] = React.useState(() => shellWindow());
  const [maximized, setMaximized] = React.useState(false);

  React.useEffect(() => {
    if (!visible || !api) return;
    let alive = true;
    const sync = () => {
      void api
        .isMaximized()
        .then((value) => {
          if (alive) setMaximized(value);
        })
        .catch(() => {
          // 拿不到状态就按「未最大化」画：图标退化成一个可点的方形，
          // 比整条标题栏消失要可接受。
        });
    };
    sync();
    const timer = window.setInterval(sync, 1000);
    return () => {
      alive = false;
      window.clearInterval(timer);
    };
  }, [visible, api]);

  if (!visible) return null;

  const button = (options: {
    label: string;
    title: string;
    icon: React.ReactNode;
    danger?: boolean;
    run?: () => void;
  }) => (
    <button
      type="button"
      aria-label={options.label}
      title={api ? options.title : "这个按钮只在桌面版里可用"}
      disabled={!api}
      onClick={options.run}
      className={cn(
        "flex h-9 w-11 items-center justify-center text-muted-foreground transition-colors",
        api ? "hover:text-foreground" : "cursor-not-allowed opacity-40",
        api && (options.danger ? "hover:bg-destructive hover:text-white" : "hover:bg-foreground/[0.06]"),
      )}
    >
      {options.icon}
    </button>
  );

  return (
    <div
      data-tauri-drag-region
      className="flex h-9 shrink-0 select-none items-center justify-end border-b border-sidebar-border bg-sidebar"
    >
      {/* 标题栏左边整段是空的：应用名与状态在侧栏里已经有了，这里只当拖拽把手用。
          空白处也要带 drag 属性 —— 它是唯一的拖拽区。 */}
      <div data-tauri-drag-region className="h-full flex-1" />

      {button({
        label: "最小化",
        title: "最小化",
        icon: <Minus className="size-4" aria-hidden="true" />,
        run: () => void api?.minimize(),
      })}
      {button({
        label: maximized ? "向下还原" : "最大化",
        title: maximized ? "向下还原" : "最大化",
        icon: maximized ? (
          <SquareStack className="size-3.5" aria-hidden="true" />
        ) : (
          <Square className="size-3.5" aria-hidden="true" />
        ),
        run: () => void api?.toggleMaximize(),
      })}
      {button({
        label: "关闭窗口",
        title: "关闭窗口（收进托盘，网关继续跑）",
        icon: <X className="size-4" aria-hidden="true" />,
        danger: true,
        run: () => void api?.close(),
      })}
    </div>
  );
}

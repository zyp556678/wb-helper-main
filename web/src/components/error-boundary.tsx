/**
 * 内容区错误边界。
 *
 * 为什么必须有它：React 在渲染期抛出未捕获异常时会**卸载整棵组件树**，
 * 表现为整个窗口白屏，而且用户拿不到任何可反馈的信息（侧栏也一起消失，
 * 连切走都做不到）。本项目已实际踩到一次：用量统计页的 `data?.rows.length`
 * 在后端返回 `rows: null` 时抛 TypeError，导致整窗白屏。
 *
 * 有了这层边界之后：
 *   · 单页异常只影响内容区，侧栏保留，用户能切到别的页面继续用；
 *   · 错误信息直接显示在界面上，可复制、可反馈，不再是无信息的白屏。
 *
 * 注意边界的能力范围：它只能捕获**渲染期**异常，捕获不到事件回调里的异常，
 * 也捕获不到异步 Promise 的拒绝。所以数据层的空值防御（`?? []`、可选链）
 * 一点都不能省 —— 这里是最后一道网，不是第一道。
 */
import { Component, type ErrorInfo, type ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // 落到控制台：桌面壳可用 WebView2 的远程调试端口读到，也便于浏览器 F12 排查。
    console.error("[面板] 渲染异常：", error, info.componentStack);
  }

  private reset = () => this.setState({ error: null });

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;

    return (
      <div className="mx-auto w-full max-w-[720px] px-6 py-10">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">这个页面出错了</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <p className="text-xs text-muted-foreground">
              其余页面仍可正常使用，用左侧导航切换即可。下面是具体错误，便于反馈定位。
            </p>
            <pre className="max-h-56 overflow-auto rounded-md border border-border/60 bg-muted/40 p-3 text-xs leading-relaxed whitespace-pre-wrap">
              {error.message}
            </pre>
            <Button size="sm" variant="outline" onClick={this.reset}>
              重试
            </Button>
          </CardContent>
        </Card>
      </div>
    );
  }
}

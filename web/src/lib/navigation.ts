import {
  Activity,
  ArrowRightLeft,
  Coins,
  HardDrive,
  Link2,
  Layers,
  BarChart3,
  ListChecks,
  ScrollText,
  Settings,
  Users,
  type LucideIcon,
} from "lucide-react";

/**
 * 侧栏导航项的 id 联合类型。后续切片继续扩页时，在这里加成员、
 * 在 NAV_ITEMS 里加一项、在 App.tsx 的页面开关里加一个分支即可。
 */
export type PageId =
  | "accounts"
  | "monitoring"
  | "models"
  | "sites"
  | "tasks"
  | "stats"
  | "credits"
  | "sessions"
  | "local"
  | "config"
  | "logs"
  | "requests";

export interface NavItem {
  id: PageId;
  label: string;
  icon: LucideIcon;
}

export const NAV_ITEMS: NavItem[] = [
  { id: "accounts", label: "账号", icon: Users },
  { id: "monitoring", label: "监控", icon: Activity },
  { id: "models", label: "模型与档位", icon: Layers },
  { id: "sites", label: "双站视图", icon: ArrowRightLeft },
  { id: "tasks", label: "任务中心", icon: ListChecks },
  { id: "stats", label: "用量统计", icon: BarChart3 },
  { id: "credits", label: "积分统计", icon: Coins },
  { id: "sessions", label: "关联会话", icon: Link2 },
  { id: "local", label: "本机客户端", icon: HardDrive },
  { id: "config", label: "配置", icon: Settings },
  { id: "logs", label: "日志", icon: ScrollText },
  { id: "requests", label: "请求流水", icon: Activity },
];

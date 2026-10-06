//! 开机自启动。
//!
//! ## 为什么默认**关**
//!
//! 桌面版和命令行安装包**共用同一个端口 8317 与同一个数据目录**。如果两者都
//! 开机自启，登录后会有两个网关抢同一个端口，后到的那个起不来，用户看到的是
//! 「有时候能连上、有时候连不上」这种最难排查的故障。所以这里只提供开关，
//! 不替用户做决定 —— 需要常驻的用户点一下开启，不需要的保持原状。
//!
//! ## 为什么不引 tauri-plugin-autostart
//!
//! 那个插件为了抹平三平台差异引了一批依赖（auto-launch 等），而我们要做的事
//! 每个平台都只有一条写文件/写注册表的路径，手写出来反而更透明、更好排查，
//! 也和这个壳「依赖表保持最小」的既定风格一致（见 Cargo.toml 顶部注释）。
//!
//! ## 各平台的落点
//!
//! | 平台    | 落点                                                    | 备注 |
//! |---------|---------------------------------------------------------|------|
//! | Windows | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`     | 值名 `WorkBuddyGatewayDesktop` |
//! | macOS   | `~/Library/LaunchAgents/com.workbuddy.gateway.desktop.plist` | |
//! | Linux   | `~/.config/autostart/workbuddy-gateway-desktop.desktop`  | |
//!
//! Windows 上刻意**不用计划任务**：命令行安装包已经在用计划任务，任务名
//! `WorkBuddyGateway`。用 `Run` 键 + 另一个值名，两者在系统里是两份独立记录，
//! 用户能分别开关、也能分别卸载，不会出现「卸了桌面版把命令行版的自启也带走」。

use std::path::PathBuf;

/// Windows「运行」键下的值名。
///
/// **与 Go 侧 `internal/autostart` 的 `winValue` 必须逐字一致** —— 面板开关走
/// Go 侧，托盘开关走这里，两者操作的是同一条记录。不一致会制造出「面板开着、
/// 托盘说关着」这种解释不清的状态，更糟的是两个自启项同时躺在系统里抢 8317。
///
/// 与命令行安装包的**计划任务** `WorkBuddyGateway` 是两套机制，刻意错开命名，
/// 卸载其中一个不会误伤另一个。
#[cfg(windows)]
const RUN_VALUE: &str = "WorkBuddyGatewayDesktop";

/// 命令行安装包用的计划任务名（`installer/windows/install.ps1`）。
/// 用来做冲突检测 —— 它和桌面版自启会抢同一个端口。
#[cfg(windows)]
const CLI_TASK: &str = "WorkBuddyGateway";

#[cfg(windows)]
const RUN_KEY: &str = r"HKCU\Software\Microsoft\Windows\CurrentVersion\Run";

/// macOS LaunchAgent 标签（与 `tauri.conf.json` 的 identifier 对齐）。
#[cfg(target_os = "macos")]
const AGENT_LABEL: &str = "com.workbuddy.gateway.desktop";

/// 自启状态。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    Enabled,
    Disabled,
    /// 当前平台没有实现（目前不该出现，留作显式兜底而不是静默当作「关」）。
    Unsupported,
}

/// 开启自启的结果。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    Ok,
    /// 命令行安装包的计划任务已存在，且会占用同一端口。
    ConflictWithCliTask,
    Failed,
}

// ---------------------------------------------------------------------------
// 公共入口
// ---------------------------------------------------------------------------

pub fn is_enabled() -> State {
    #[cfg(windows)]
    {
        return windows_state();
    }
    #[cfg(target_os = "macos")]
    {
        return if macos_plist_path().is_some_and(|p| p.is_file()) {
            State::Enabled
        } else {
            State::Disabled
        };
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        return if linux_desktop_path().is_some_and(|p| p.is_file()) {
            State::Enabled
        } else {
            State::Disabled
        };
    }
    #[allow(unreachable_code)]
    State::Unsupported
}

/// 开启自启。启用前先检查与命令行版计划任务的冲突。
pub fn enable() -> Outcome {
    if cli_task_present() {
        return Outcome::ConflictWithCliTask;
    }
    if write_autostart_entry() {
        Outcome::Ok
    } else {
        Outcome::Failed
    }
}

/// 关闭自启。
pub fn disable() -> Outcome {
    if remove_autostart_entry() {
        Outcome::Ok
    } else {
        Outcome::Failed
    }
}

/// 清理**命令行版**的计划任务（用户在冲突提示里选择「用桌面版」时调用）。
///
/// 只删那一个具名任务，不动任何其它计划任务。
pub fn remove_cli_task() -> bool {
    #[cfg(windows)]
    {
        // /F 不再二次确认；任务不存在时会返回非零，不影响调用方判断成功与否，
        // 因为紧接着还会做一次 cli_task_present() 复核。
        let out = hidden_output("schtasks", &["/Delete", "/TN", CLI_TASK, "/F"]);
        let _ = out;
        return !cli_task_present();
    }
    #[allow(unreachable_code)]
    false
}

/// 系统里是否存在命令行安装包设置的自启（计划任务）。
///
/// 这是唯一的冲突来源：本开关只用一个注册表值名（`RUN_VALUE`），
/// 注册表侧不可能与自己冲突。安装包的计划任务则会在登录时拉起**另一个**
/// 网关进程，与我们抢 8317 端口。
pub fn cli_task_present() -> bool {
    #[cfg(windows)]
    {
        let out = hidden_output("schtasks", &["/Query", "/TN", CLI_TASK]);
        // 任务存在时 schtasks 退出码为 0；不存在时非零（且 stderr 有「找不到」）。
        // 只看退出码即可：不同语言的 Windows 提示文案不一样，解析文本会误判。
        return out.is_some_and(|o| o.0);
    }
    #[allow(unreachable_code)]
    false
}

// ---------------------------------------------------------------------------
// Windows：HKCU\...\Run
// ---------------------------------------------------------------------------

/// 查询 `Run` 键里是否存在自启值。
#[cfg(windows)]
fn windows_state() -> State {
    match hidden_output("reg", &["query", RUN_KEY, "/v", RUN_VALUE]) {
        Some((true, text)) if !text.trim().is_empty() => State::Enabled,
        _ => State::Disabled,
    }
}

#[cfg(windows)]
fn write_autostart_entry() -> bool {
    let Some(exe) = current_exe_path() else {
        return false;
    };
    // 用 reg add 而不是直接写注册表：与卸载脚本（uninstall.ps1 里的 reg delete）
    // 走同一套工具，行为可对照。`/d` 的值加引号，路径含空格时才能被正确解析。
    let value = format!("\"{}\"", exe.display());
    hidden_output(
        "reg",
        &[
            "add", RUN_KEY, "/v", RUN_VALUE, "/t", "REG_SZ", "/d", &value, "/f",
        ],
    )
    .is_some_and(|o| o.0)
}

#[cfg(windows)]
fn remove_autostart_entry() -> bool {
    let _ = hidden_output("reg", &["delete", RUN_KEY, "/v", RUN_VALUE, "/f"]);
    // 值本来就不存在时也算成功（幂等）：调用方关心的是「现在没有自启项」这个结果。
    windows_state() == State::Disabled
}

// ---------------------------------------------------------------------------
// macOS：LaunchAgent plist
// ---------------------------------------------------------------------------

#[cfg(target_os = "macos")]
fn macos_plist_path() -> Option<PathBuf> {
    let home = std::env::var_os("HOME")?;
    Some(
        PathBuf::from(home)
            .join("Library")
            .join("LaunchAgents")
            .join(format!("{AGENT_LABEL}.plist")),
    )
}

#[cfg(target_os = "macos")]
fn write_autostart_entry() -> bool {
    let Some(path) = macos_plist_path() else {
        return false;
    };
    let Some(exe) = current_exe_path() else {
        return false;
    };
    if let Some(parent) = path.parent() {
        if std::fs::create_dir_all(parent).is_err() {
            return false;
        }
    }
    // RunAtLoad 让登录时自动拉起。不加 KeepAlive：壳本身已经管着网关子进程，
    // 让 launchd 再去守护「壳」会导致用户从托盘点「退出」后又被立刻拉起来。
    let plist = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{AGENT_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{exe}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
"#,
        exe = exe.display()
    );
    if std::fs::write(&path, plist).is_err() {
        return false;
    }
    // 立即装载：不重登也能生效（plist 已存在时 bootstrap 会报错，此时先 bootout）。
    let home = std::env::var("HOME").unwrap_or_default();
    let _ = std::process::Command::new("launchctl")
        .args(["bootout", &format!("gui/{}", uid())])
        .arg(&path)
        .status();
    let _ = std::process::Command::new("launchctl")
        .args(["bootstrap", &format!("gui/{}", uid())])
        .arg(&path)
        .status();
    let _ = home;
    true
}

#[cfg(target_os = "macos")]
fn remove_autostart_entry() -> bool {
    let Some(path) = macos_plist_path() else {
        return false;
    };
    let _ = std::process::Command::new("launchctl")
        .args(["bootout", &format!("gui/{}", uid())])
        .arg(&path)
        .status();
    let _ = std::fs::remove_file(&path);
    !path.exists()
}

#[cfg(target_os = "macos")]
fn uid() -> u32 {
    // 用 id -u 取真实 uid：不引 libc，也不猜。
    std::process::Command::new("id")
        .arg("-u")
        .output()
        .ok()
        .and_then(|o| String::from_utf8_lossy(&o.stdout).trim().parse().ok())
        .unwrap_or(501)
}

// ---------------------------------------------------------------------------
// Linux：XDG autostart .desktop
// ---------------------------------------------------------------------------

#[cfg(all(unix, not(target_os = "macos")))]
fn linux_desktop_path() -> Option<PathBuf> {
    let base = std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".config")))?;
    Some(base.join("autostart").join("workbuddy-gateway-desktop.desktop"))
}

#[cfg(all(unix, not(target_os = "macos")))]
fn write_autostart_entry() -> bool {
    let Some(path) = linux_desktop_path() else {
        return false;
    };
    let Some(exe) = current_exe_path() else {
        return false;
    };
    if let Some(parent) = path.parent() {
        if std::fs::create_dir_all(parent).is_err() {
            return false;
        }
    }
    let body = format!(
        "[Desktop Entry]\n\
         Type=Application\n\
         Name=WorkBuddy 网关\n\
         Comment=开机自动启动 WorkBuddy 网关桌面版\n\
         Exec={}\n\
         Terminal=false\n\
         X-GNOME-Autostart-enabled=true\n",
        exe.display()
    );
    std::fs::write(&path, body).is_ok()
}

#[cfg(all(unix, not(target_os = "macos")))]
fn remove_autostart_entry() -> bool {
    let Some(path) = linux_desktop_path() else {
        return false;
    };
    let _ = std::fs::remove_file(&path);
    !path.exists()
}

// ---------------------------------------------------------------------------
// 共用工具
// ---------------------------------------------------------------------------

/// 当前可执行文件路径。
///
/// 用 `current_exe()` 而不是 `argv[0]`：后者可能是相对路径或经 PATH 解析的名字，
/// 写进自启项后在登录时的工作目录下未必能解析到。
fn current_exe_path() -> Option<PathBuf> {
    std::env::current_exe().ok()
}

/// 跑一条命令并收下 (是否成功, stdout+stderr)，全程不弹窗口。
///
/// 返回 `None` 表示命令压根没起来（找不到可执行文件等）。
///
/// 只有 Windows 分支（`reg` / `schtasks`）与单元测试会用到它；macOS / Linux 的
/// 自启落点是直接写文件，不需要起子进程。不加这个属性的话，Linux 上构建会得到
/// 一条 `function is never used` 警告 —— 那会掩盖真正需要注意的警告。
#[cfg_attr(not(any(windows, test)), allow(dead_code))]
fn hidden_output(program: &str, args: &[&str]) -> Option<(bool, String)> {
    let mut cmd = std::process::Command::new(program);
    cmd.args(args);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        /// 托盘应用调用 reg.exe / schtasks.exe 时不该闪黑框。
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    let out = cmd.output().ok()?;
    let mut text = String::from_utf8_lossy(&out.stdout).to_string();
    text.push_str(&String::from_utf8_lossy(&out.stderr));
    Some((out.status.success(), text))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 状态查询在**未开启**时不应报 Enabled。
    ///
    /// 自动化环境里通常没开过自启，这条能挡住「查询逻辑写反」这类低级错误。
    /// 如果开发机恰好开着自启，这条会失败 —— 那时请先关掉再跑测试。
    #[test]
    fn state_is_disabled_by_default_in_test_env() {
        let state = is_enabled();
        assert!(
            matches!(state, State::Disabled | State::Unsupported),
            "未配置自启时不应报 Enabled，实际 {state:?}"
        );
    }

    /// 可执行文件路径必须能取到，否则自启项会写成空路径。
    #[test]
    fn current_exe_is_resolvable() {
        let exe = current_exe_path().expect("测试进程也应能取到自身路径");
        assert!(exe.is_absolute(), "自启项必须写绝对路径，实际 {exe:?}");
    }
}

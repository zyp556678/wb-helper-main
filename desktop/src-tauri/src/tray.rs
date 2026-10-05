//! 托盘图标、菜单与窗口显隐策略。
//!
//! 这里决定了一个产品行为：**关闭窗口 ≠ 退出应用**。网关的核心价值是常驻后台
//! 给 CLI / IDE 提供 API，关窗就把服务停掉会让桌面版反而弱于命令行版。
//! 因此关闭动作被解释为"收进托盘"，真正的退出只走托盘菜单的「退出」。

use tauri::menu::{CheckMenuItem, Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, Window, WindowEvent};

use crate::autostart::{self, Outcome, State};
use crate::backend;

/// 托盘里「开机自启动」那一项。
///
/// 存下来是为了切换后能直接回填勾选态 —— `TrayIcon` 没有 menu getter，
/// 拿不到句柄就只能靠重建菜单，那会丢掉其它项的状态。
struct AutostartItem(CheckMenuItem<tauri::Wry>);

pub fn setup(app: &AppHandle) -> tauri::Result<()> {
    let show = MenuItem::with_id(app, "show", "显示面板", true, None::<&str>)?;
    let browser = MenuItem::with_id(app, "browser", "在浏览器中打开", true, None::<&str>)?;
    let restart = MenuItem::with_id(app, "restart", "重新启动网关", true, None::<&str>)?;
    let data = MenuItem::with_id(app, "data", "打开数据目录", true, None::<&str>)?;

    // 勾选态来自系统里的真实状态，不是内存里的标志位：
    // 用户可能在「任务管理器 → 启动」里把它关掉，或者被第三方清理工具改过，
    // 只信自己记的会显示成与实际相反的样子。
    let autostart_item = CheckMenuItem::with_id(
        app,
        "autostart",
        "开机自启动",
        true,
        autostart::is_enabled() == State::Enabled,
        None::<&str>,
    )?;
    // 交给 Tauri 托管，事件回调里按类型取回（见 refresh_autostart_item）。
    app.manage(AutostartItem(autostart_item.clone()));

    let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;

    let menu = Menu::with_items(
        app,
        &[
            &show,
            &browser,
            &restart,
            &PredefinedMenuItem::separator(app)?,
            &autostart_item,
            &data,
            &PredefinedMenuItem::separator(app)?,
            &quit,
        ],
    )?;

    let mut builder = TrayIconBuilder::with_id("main-tray")
        .tooltip("WorkBuddy 网关")
        .menu(&menu)
        // 左键单击交给"显示窗口"，菜单只在右键出现 —— 这是 Windows 托盘的一般预期。
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "show" => show_main_window(app),
            "browser" => open_in_browser(),
            "restart" => backend::restart(app.clone(), backend::DEFAULT_PORT),
            "autostart" => toggle_autostart(app),
            "data" => open_path(&backend::data_dir_path()),
            "quit" => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                show_main_window(tray.app_handle());
            }
        });

    if let Some(icon) = app.default_window_icon() {
        builder = builder.icon(icon.clone());
    }
    builder.build(app)?;
    Ok(())
}

/// 关闭窗口时收进托盘，而不是退出进程。
///
/// 用 `prevent_close` + `hide` 而不是销毁窗口：面板里可能正开着表单或长任务视图，
/// 隐藏后再显示能保留完整状态，重建窗口则要重新拉一遍全量数据。
pub fn on_window_event(window: &Window, event: &WindowEvent) {
    if let WindowEvent::CloseRequested { api, .. } = event {
        api.prevent_close();
        let _ = window.hide();
    }
}

pub fn show_main_window(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}

/// 切换开机自启动。
///
/// 勾选态以**系统真实状态**为准（读注册表 / plist / .desktop），因此这里不沿用
/// 菜单项传来的选中态，而是先读一次现状再取反 —— 否则用户在「任务管理器 → 启动」
/// 里手动改过之后，菜单的勾与实际会一直错位。
///
/// 开启前会检查命令行安装包的计划任务：两者共用 8317 端口，同时自启必然有一个
/// 起不来，且表现为「有时能连有时连不上」，所以宁可当场问清楚。
fn toggle_autostart(app: &AppHandle) {
    match autostart::is_enabled() {
        State::Enabled => {
            let outcome = autostart::disable();
            refresh_autostart_item(app);
            if outcome == Outcome::Failed {
                notify(
                    app,
                    "关闭开机自启动失败",
                    "无法移除自启项，请检查系统权限后重试。",
                );
            }
        }
        State::Disabled => {
            let outcome = autostart::enable();
            match outcome {
                Outcome::Ok => {}
                Outcome::Failed => notify(
                    app,
                    "开启开机自启动失败",
                    "无法写入自启项，请检查系统权限后重试。",
                ),
                Outcome::ConflictWithCliTask => {
                    // 冲突处置交给用户：桌面版和命令行版都能自启，但只能有一个，
                    // 替用户选等于悄悄关掉他可能正依赖的那一个。
                    let use_desktop = confirm(
                        app,
                        "检测到命令行版已设置开机自启",
                        "命令行版（计划任务 WorkBuddyGateway）已设置为开机启动。\n\
                         它与桌面版共用同一个端口（8317），两个同时自启会互相抢端口，\n\
                         表现为「有时能连上、有时连不上」。\n\n\
                         是否改为使用桌面版？\n\
                         选择「是」将删除命令行版的自启计划任务，并启用桌面版自启。\n\
                         选择「否」则保持现状，桌面版不设置自启。",
                        "改为使用桌面版",
                        "保持命令行版",
                    );
                    if use_desktop && autostart::remove_cli_task() {
                        if autostart::enable() != Outcome::Ok {
                            notify(app, "开启开机自启动失败", "已移除命令行版任务，但写入自启项失败。");
                        }
                    }
                }
            }
            // 无论走哪条分支都以系统真实状态回填勾：冲突时用户选了「保持命令行版」，
            // 勾必须回到未选中，不能停在用户刚点的那一下。
            refresh_autostart_item(app);
        }
        State::Unsupported => notify(
            app,
            "当前系统不支持",
            "此平台尚未实现开机自启动管理。",
        ),
    }
}

/// 用系统真实状态回填菜单勾选态。
fn refresh_autostart_item(app: &AppHandle) {
    let enabled = autostart::is_enabled() == State::Enabled;
    let Some(item) = app.try_state::<AutostartItem>() else {
        return;
    };
    let _ = item.0.set_checked(enabled);
}

/// 是 / 否 对话框。返回用户是否选了「是」。
fn confirm(app: &AppHandle, title: &str, body: &str, yes: &str, no: &str) -> bool {
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogKind};
    app.dialog()
        .message(body)
        .title(title)
        .kind(MessageDialogKind::Info)
        .buttons(MessageDialogButtons::OkCancelCustom(
            yes.to_string(),
            no.to_string(),
        ))
        .blocking_show()
}

/// 单向提示对话框（只有「确定」）。
fn notify(app: &AppHandle, title: &str, body: &str) {
    use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
    app.dialog()
        .message(body)
        .title(title)
        .kind(MessageDialogKind::Warning)
        .blocking_show();
}

/// 用系统默认浏览器打开面板。
///
/// 面板与 API 同源，浏览器形态与壳内形态是同一份界面，因此"在浏览器中打开"
/// 不是降级而是另一种入口（例如用户想用浏览器插件、或多屏并排）。
///
/// 带上密钥（fragment 形态）：网关现在会校验，不带的话浏览器里打开只会看到
/// 一片 401。fragment 不发给服务器，且面板读到后会立刻从地址栏和历史里抹掉。
fn open_in_browser() {
    let url = backend::current_panel_url(backend::DEFAULT_PORT);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        let _ = std::process::Command::new("cmd")
            .args(["/C", "start", "", &url])
            .creation_flags(CREATE_NO_WINDOW)
            .spawn();
    }
    #[cfg(target_os = "macos")]
    {
        let _ = std::process::Command::new("open").arg(&url).spawn();
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        let _ = std::process::Command::new("xdg-open").arg(&url).spawn();
    }
}

fn open_path(path: &std::path::Path) {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        let _ = std::process::Command::new("explorer")
            .arg(path)
            .creation_flags(CREATE_NO_WINDOW)
            .spawn();
    }
    #[cfg(target_os = "macos")]
    {
        let _ = std::process::Command::new("open").arg(path).spawn();
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        let _ = std::process::Command::new("xdg-open").arg(path).spawn();
    }
}

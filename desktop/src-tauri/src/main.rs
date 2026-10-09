// 托盘应用不该带控制台窗口；debug 构建保留，便于看 panic 与日志。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod api_key;
mod autostart;
mod backend;
mod tray;

/// 自启项里带的「静默启动」参数：只驻托盘，不显示面板窗口。
///
/// 与 Go 侧 `internal/autostart` 的 `SilentArg` **必须逐字一致**：自启项由两侧
/// 共同写入（面板开关走 Go，托盘开关走 Rust），不一致就会出现「从面板开是静默、
/// 从托盘开是弹窗」这种解释不清的差异。
const SILENT_ARG: &str = "--silent";

fn main() {
    let mut builder = tauri::Builder::default();

    // 托盘里切换「开机自启动」时的确认框要用到（见 tray::toggle_autostart）。
    builder = builder.plugin(tauri_plugin_dialog::init());

    // 是不是被自启项拉起来的。用户关机时并没有开着这个窗口，开机却弹出一个面板，
    // 属于「每次开机都要手动关掉」的骚扰 —— 所以开机自启一律静默驻留托盘，
    // 想看面板的人点托盘图标。手动双击启动不带这个参数，行为与以前一致。
    let silent = std::env::args().skip(1).any(|arg| arg == SILENT_ARG);

    // 单实例必须最先注册：第二个实例在此之前退出，
    // 不会建窗口、建托盘，更不会再去拉起一个网关抢端口。
    #[cfg(desktop)]
    {
        builder = builder.plugin(tauri_plugin_single_instance::init(|app, argv, _cwd| {
            // 已经在跑的那个实例收到了这次启动请求。只有用户主动启动才该把窗口拉到
            // 前面；静默启动（自启项触发，例如 launchctl 重新 bootstrap）不该弹窗，
            // 否则「静默」就只在第一次登录时成立。
            if !argv.iter().any(|arg| arg.as_str() == SILENT_ARG) {
                tray::show_main_window(app);
            }
        }));
    }

    builder
        .setup(move |app| {
            tray::setup(app.handle())?;
            if !silent {
                tray::show_main_window(app.handle());
            }

            // 启动后端要等就绪，可能耗时数十秒，必须放后台线程：
            // 卡在 setup 里会让窗口在就绪前完全无响应。
            let handle = app.handle().clone();
            std::thread::spawn(move || backend::boot(handle, backend::DEFAULT_PORT));
            Ok(())
        })
        .on_window_event(|window, event| tray::on_window_event(window, event))
        .build(tauri::generate_context!())
        .expect("构建 Tauri 应用失败")
        .run(|_app, event| {
            // 退出前收掉自己拉起的网关，不留孤儿进程占着端口。
            if let tauri::RunEvent::Exit = event {
                backend::shutdown();
            }
        });
}

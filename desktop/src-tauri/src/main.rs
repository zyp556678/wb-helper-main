// 托盘应用不该带控制台窗口；debug 构建保留，便于看 panic 与日志。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod api_key;
mod autostart;
mod backend;
mod tray;

fn main() {
    let mut builder = tauri::Builder::default();

    // 托盘里切换「开机自启动」时的确认框要用到（见 tray::toggle_autostart）。
    builder = builder.plugin(tauri_plugin_dialog::init());

    // 单实例必须最先注册：第二个实例在此之前退出，
    // 不会建窗口、建托盘，更不会再去拉起一个网关抢端口。
    #[cfg(desktop)]
    {
        builder = builder.plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            tray::show_main_window(app);
        }));
    }

    builder
        .setup(|app| {
            tray::setup(app.handle())?;
            tray::show_main_window(app.handle());

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

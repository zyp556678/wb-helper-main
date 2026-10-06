//! Go 网关进程的代管：定位二进制、探测既有实例、启动、等待就绪、退出时收尾。
//!
//! 这里的所有网络探测都用 `std::net` 手写 HTTP，不引入 HTTP 客户端依赖 ——
//! 我们只需要判断 `/healthz` 是否返回 200，几十行就够，换来的是少编译上百个 crate。

use std::io::{Read, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant};

use tauri::{AppHandle, Manager};

/// 默认端口：与 CLI / 安装包 / Docker 完全一致，避免"桌面版和命令行版各占一个口"。
pub const DEFAULT_PORT: u16 = 8317;

/// 网关启动的最长等待时间。首次启动要读配置、校验凭据、拉模型目录，
/// 冷启动实测在数秒内，60 秒是留给"上游探测卡住"这类慢路径的余量。
const READY_TIMEOUT: Duration = Duration::from_secs(60);

/// 由本壳启动的子进程。`None` 表示当前复用的是**外部已有实例**（不是我们起的，
/// 退出时也不该由我们关掉）。
static CHILD: OnceLock<Mutex<Option<Child>>> = OnceLock::new();

fn child_slot() -> &'static Mutex<Option<Child>> {
    CHILD.get_or_init(|| Mutex::new(None))
}

/// 本次启动是否由本壳拉起后端（决定退出时是否收进程）。
static OWNS_BACKEND: OnceLock<bool> = OnceLock::new();

pub fn owns_backend() -> bool {
    *OWNS_BACKEND.get_or_init(|| false)
}

/// 向 `/healthz` 发一次极简 HTTP 请求，判断网关是否已就绪。
///
/// 只读取前 64 字节判断状态行里是否含 " 200"。用 `HTTP/1.0` + `Connection: close`
/// 让服务端在回完响应后主动断开，不用处理 chunked 与长度。
pub fn probe(port: u16) -> bool {
    let addr = SocketAddr::from(([127, 0, 0, 1], port));
    let mut stream = match TcpStream::connect_timeout(&addr, Duration::from_millis(400)) {
        Ok(stream) => stream,
        Err(_) => return false,
    };
    let _ = stream.set_read_timeout(Some(Duration::from_millis(800)));
    let request = format!(
        "GET /healthz HTTP/1.0\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n"
    );
    if stream.write_all(request.as_bytes()).is_err() {
        return false;
    }
    let mut buf = [0u8; 64];
    match stream.read(&mut buf) {
        Ok(n) if n > 0 => {
            let head = String::from_utf8_lossy(&buf[..n]);
            head.starts_with("HTTP/") && head.contains(" 200")
        }
        _ => false,
    }
}

/// 面板地址。
/// 面板地址（不带密钥）。仅用于「不知道密钥」的场景。
pub fn panel_url(port: u16) -> String {
    format!("http://127.0.0.1:{port}/panel/")
}

/// 面板地址，带上已有的接入密钥。
///
/// 只有密钥文件**已经存在**时才附加：那说明是我们上次启动网关时配的，
/// 注入它能让面板免于手动输入密钥。文件不存在就不凭空造一个去注入 ——
/// 那个外部启动的网关用的不是这个密钥，注入了反而会让人误以为配好了。
pub fn current_panel_url(port: u16) -> String {
    match existing_api_key() {
        Some(key) => crate::api_key::panel_url_with_key(port, &key),
        None => panel_url(port),
    }
}

/// 读取**已有**的密钥文件；不存在或为空则返回 None（不生成）。
fn existing_api_key() -> Option<String> {
    let path = data_dir().join(crate::api_key::API_KEY_FILE);
    std::fs::read_to_string(path)
        .ok()
        .map(|raw| raw.trim().to_string())
        .filter(|key| !key.is_empty())
}

/// 定位同目录下的网关二进制。
///
/// 查找顺序（先近后远）：
///   1. 可执行文件同目录          —— 安装包 / 便携包布局
///   2. 可执行文件同目录 bin/     —— Tauri 资源目录布局（Windows 每用户安装）
///   3. `extra_dirs`              —— Tauri 的 `resource_dir()`，见 `resource_dirs()`
///   4. 开发布局：向上找到项目根   —— `cargo run` 时 exe 在 target/ 深处
fn locate(name: &str, extra_dirs: &[PathBuf]) -> Option<PathBuf> {
    let mut dirs: Vec<PathBuf> = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(parent) = exe.parent() {
            dirs.push(parent.to_path_buf());
            dirs.push(parent.join("bin"));
        }
    }
    dirs.extend(extra_dirs.iter().cloned());
    if let Ok(exe) = std::env::current_exe() {
        if let Some(parent) = exe.parent() {
            // 开发时 target/<profile>/ 距项目根有 3 层
            let mut cursor = parent.to_path_buf();
            for _ in 0..4 {
                if let Some(up) = cursor.parent() {
                    cursor = up.to_path_buf();
                    dirs.push(cursor.clone());
                }
            }
        }
    }
    if let Ok(cwd) = std::env::current_dir() {
        dirs.push(cwd.clone());
        dirs.push(cwd.join(".."));
    }
    for dir in dirs {
        let candidate = dir.join(name);
        if candidate.is_file() {
            return Some(candidate);
        }
    }
    None
}

/// Tauri 声明的资源目录（`bundle.resources` 的落点），以及它下面的 `bin/`。
///
/// 为什么必须单独向 Tauri 要这两个路径：Linux 的 deb 布局把壳装在 `/usr/bin/`，
/// 而资源被放到 `/usr/lib/<产品名>/` —— **两者不同级**，靠「exe 同目录 / 上一级」
/// 这类相对路径永远找不到。少了这一步，装完打开就是
/// 「未找到 workbuddy-gateway 可执行文件」，而这在打包期毫无征兆。
///
/// Windows 上 `resource_dir()` 等于 exe 同目录，与第 1、2 条候选重合，无副作用。
fn resource_dirs(app: &AppHandle) -> Vec<PathBuf> {
    let mut dirs = Vec::new();
    if let Ok(dir) = app.path().resource_dir() {
        dirs.push(dir.join("bin"));
        dirs.push(dir);
    }
    dirs
}

/// 数据目录的**目录名**。
///
/// 两边命名有意不同，不是笔误：Windows 的约定是 `%LOCALAPPDATA%\wb-gateway`
/// （不隐藏），而 macOS / Linux 的约定是 `~/.wb-gateway`（点开头，算隐藏目录）——
/// Go 网关自己的默认值、npm 入口、DEPLOY.md 与 README 都按这个口径写。
///
/// 少了这个点，壳会把数据写进主目录下一个显眼的 `~/wb-gateway`，而命令行版仍读
/// `~/.wb-gateway`，现象正好是「装完桌面版，之前登录的账号全没了」—— 与
/// `base_data_dir()` 要修的是同一类问题，只是更隐蔽：路径看着完全合理。
#[cfg(windows)]
const DATA_DIR_NAME: &str = "wb-gateway";
#[cfg(not(windows))]
const DATA_DIR_NAME: &str = ".wb-gateway";

/// 网关数据目录：与安装包/npm 入口保持同一处，保证「桌面版看到的账号池」
/// 和「命令行版看到的」是同一份，不会各存一份让人困惑。
fn data_dir() -> PathBuf {
    if let Ok(explicit) = std::env::var("WB_GATEWAY_DATA_DIR") {
        if !explicit.trim().is_empty() {
            return PathBuf::from(explicit);
        }
    }
    base_data_dir().join(DATA_DIR_NAME)
}

/// 各平台的「用户数据根目录」。
///
/// Windows 取 `%LOCALAPPDATA%`；macOS / Linux 取 `$HOME` —— 与 Go 网关自己的
/// 默认值、npm 入口落在同一处，三个入口共用同一份账号池。
///
/// 为什么要显式分出非 Windows 分支：原来两边都写 `LOCALAPPDATA` 取不到就回退
/// `temp_dir()`，在 Linux 上意味着数据目录变成 `/tmp/wb-gateway` —— 重启即丢，
/// 且与 README 承诺的 `~/.wb-gateway` 不符，用户看到的现象是「装完桌面版，
/// 之前命令行版登录的账号全没了」。只有在连 `HOME` 都取不到的退化环境里
/// 才用临时目录兜底，保证仍能启动而不是直接崩。
#[cfg(windows)]
fn base_data_dir() -> PathBuf {
    std::env::var("LOCALAPPDATA")
        .map(PathBuf::from)
        .unwrap_or_else(|_| std::env::temp_dir())
}

#[cfg(not(windows))]
fn base_data_dir() -> PathBuf {
    std::env::var("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|_| std::env::temp_dir())
}

#[cfg(windows)]
fn configure_no_window(cmd: &mut Command) {
    use std::os::windows::process::CommandExt;
    /// 不弹控制台窗口。否则点桌面图标会先闪一个黑框，托盘应用不该这样。
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    cmd.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(not(windows))]
fn configure_no_window(_cmd: &mut Command) {}

/// 启动网关子进程。
///
/// `api_key` 会以 `--api-key` 传给网关，让它对 `/v1/*` 与面板接口做校验。
/// 为什么桌面版也要校验：网关在本机代理的是**付费账号**，不校验时本机任何
/// 进程都能白用；而且用户要把这个密钥填进别的工具，那就得是真的在校验的。
fn spawn_gateway(bin: &Path, port: u16, api_key: &str) -> std::io::Result<Child> {
    let dir = data_dir();
    let _ = std::fs::create_dir_all(&dir);
    let log_dir = dir.join("logs");
    let _ = std::fs::create_dir_all(&log_dir);

    // 日志重定向到文件：壳是托盘应用、没有控制台，网关的 stdout/stderr 若丢弃，
    // 后台启动失败就完全无法排查。
    let log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(log_dir.join("gateway.log"))?;
    let log_err = log.try_clone()?;

    let mut cmd = Command::new(bin);
    cmd.arg("serve")
        .arg("--port")
        .arg(port.to_string())
        .arg("--api-key")
        .arg(api_key)
        .current_dir(bin.parent().unwrap_or(Path::new(".")))
        .env("WB_GATEWAY_DATA_DIR", &dir)
        // 声明「stdin 是壳给的管道」，让网关在管道关闭（=壳已消亡）时自行退出。
        //
        // 为什么需要这条：壳正常退出会走 RunEvent::Exit → backend::shutdown() 杀子进程，
        // 但壳被**强制结束**时（安装程序的 taskkill、任务管理器结束任务、壳崩溃）
        // 那段代码不会执行，网关就成了孤儿，继续占着 bin\workbuddy-gateway.exe，
        // 表现为「安装时提示先退出软件、点了确定仍报文件被占用」。
        //
        // 用环境变量显式开启而不是让网关自己判断 stdin 是不是管道：命令行与容器
        // 场景下 stdin 常是 /dev/null 或已关闭句柄，自动判断会让网关立刻退出。
        .env("WB_GATEWAY_PARENT_WATCH", "1")
        .stdin(Stdio::piped())
        .stdout(Stdio::from(log))
        .stderr(Stdio::from(log_err));
    configure_no_window(&mut cmd);
    cmd.spawn()
}

fn set_status(app: &AppHandle, text: &str) {
    if let Some(win) = app.get_webview_window("main") {
        let payload = serde_json::to_string(text).unwrap_or_else(|_| "\"\"".into());
        let _ = win.eval(&format!(
            "window.__wbStatus && window.__wbStatus('progress', {payload});"
        ));
    }
}

fn set_error(app: &AppHandle, text: &str) {
    if let Some(win) = app.get_webview_window("main") {
        let payload = serde_json::to_string(text).unwrap_or_else(|_| "\"\"".into());
        let _ = win.eval(&format!(
            "window.__wbStatus && window.__wbStatus('error', {payload});"
        ));
    }
}

/// 就绪后把窗口导航到面板。
///
/// `api_key` 为 `Some` 时会编进 URL 的 fragment 交给面板（见
/// `api_key::panel_url_with_key`）：fragment 不发给服务器，面板脚本读到后
/// 立刻从地址栏和历史里抹掉，于是用户不必手动输入密钥。
fn navigate_to_panel(app: &AppHandle, port: u16, api_key: Option<&str>) {
    if let Some(win) = app.get_webview_window("main") {
        let url = match api_key {
            Some(key) => crate::api_key::panel_url_with_key(port, key),
            None => panel_url(port),
        };
        match url.parse() {
            Ok(url) => {
                if let Err(err) = win.navigate(url) {
                    set_error(app, &format!("打开面板失败：{err}"));
                }
            }
            Err(err) => set_error(app, &format!("面板地址无效：{err}")),
        }
    }
}

/// 启动流程（在后台线程执行，不阻塞事件循环）：
///   已就绪 → 直接复用；否则拉起 → 轮询就绪 → 导航。
pub fn boot(app: AppHandle, port: u16) {
    if probe(port) {
        // 已经有网关在跑（用户先前用 CLI / 安装包启动过，或上一个壳没退干净）。
        // 直接复用而不是报"端口被占用"——后者会让用户面对一个打不开的应用。
        //
        // 复用时不生成密钥（那个进程用的不是我们生成的），只在**已有密钥文件**时
        // 注入一次：那是我们上次启动时配的，能命中就省掉手动输入，命中不了
        // 面板会自己弹出输入框，没有副作用。
        set_status(&app, "检测到已运行的网关，正在连接…");
        navigate_to_panel(&app, port, existing_api_key().as_deref());
        return;
    }

    let extra_dirs = resource_dirs(&app);
    let Some(bin) = locate("workbuddy-gateway.exe", &extra_dirs)
        .or_else(|| locate("workbuddy-gateway", &extra_dirs))
    else {
        set_error(
            &app,
            "未找到 workbuddy-gateway 可执行文件。\n请把它与本程序放在同一目录，或使用完整安装包。",
        );
        return;
    };

    // 接入密钥：首次运行生成并落盘，之后一直复用。
    // 复用而不是每次重生成，是因为用户会把它填进 Claude Code / Cursor 之类的外部工具，
    // 每次启动换一个会让那些配置全部失效。
    let dir = data_dir();
    let api_key = match crate::api_key::ensure_api_key(&dir) {
        Ok(key) => key,
        Err(err) => {
            // 把路径也报出来：这条错误只在磁盘/权限异常时出现，
            // 没有路径的话用户和我们都无从下手。
            set_error(
                &app,
                &format!("生成接入密钥失败：{err}\n路径：{}", dir.display()),
            );
            return;
        }
    };

    set_status(&app, "正在启动网关服务…");
    match spawn_gateway(&bin, port, &api_key) {
        Ok(child) => {
            *child_slot().lock().unwrap() = Some(child);
            let _ = OWNS_BACKEND.set(true);
        }
        Err(err) => {
            set_error(&app, &format!("启动网关失败：{err}"));
            return;
        }
    }

    let started = Instant::now();
    loop {
        if probe(port) {
            navigate_to_panel(&app, port, Some(&api_key));
            return;
        }
        // 子进程若已退出（配置错误、端口冲突、二进制损坏），没必要空等到超时。
        if let Some(child) = child_slot().lock().unwrap().as_mut() {
            if let Ok(Some(status)) = child.try_wait() {
                set_error(
                    &app,
                    &format!(
                        "网关进程已退出（{status}）。\n详情见日志：{}",
                        data_dir().join("logs").join("gateway.log").display()
                    ),
                );
                return;
            }
        }
        if started.elapsed() > READY_TIMEOUT {
            set_error(&app, "网关启动超时（60 秒）。请查看日志文件后重试。");
            return;
        }
        std::thread::sleep(Duration::from_millis(400));
    }
}

/// 退出时收掉由本壳拉起的网关。
///
/// 只关自己启动的那个：复用外部实例时 `CHILD` 为 `None`，此时绝不能去杀
/// 一个我们没启动的进程（那是用户自己的服务）。
pub fn shutdown() {
    if let Some(slot) = CHILD.get() {
        if let Ok(mut guard) = slot.lock() {
            if let Some(mut child) = guard.take() {
                let _ = child.kill();
                let _ = child.wait();
            }
        }
    }
}

/// 「重启网关」：先收掉自己起的实例，再重新走一遍启动流程。
pub fn restart(app: AppHandle, port: u16) {
    shutdown();
    // 外部实例无法被我们重启（不是我们的进程），如实告知而不是假装重启了。
    if !owns_backend() && probe(port) {
        set_status(&app, "当前网关由外部启动，桌面端无法重启它。");
        navigate_to_panel(&app, port, existing_api_key().as_deref());
        return;
    }
    std::thread::spawn(move || boot(app, port));
}

/// 数据目录路径（供托盘「打开数据目录」使用）。
pub fn data_dir_path() -> PathBuf {
    data_dir()
}

//! Go 网关进程的代管：定位二进制、探测既有实例、启动、等待就绪、退出时收尾。
//!
//! 这里的所有网络探测都用 `std::net` 手写 HTTP，不引入 HTTP 客户端依赖 ——
//! 我们只需要判断 `/healthz` 是否返回 200，几十行就够，换来的是少编译上百个 crate。

use std::io::{Read, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
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

/// 读取运行中网关的版本（`/healthz` 的 `version` 字段）。
///
/// 与 `probe` 分开：那个只看状态行、读 64 字节就够；这里要的是响应体内容，
/// 因此读完整个响应（上限 8 KiB —— 真实响应只有几百字节，超出说明对面不是我们）。
fn running_gateway_version(port: u16) -> Option<String> {
    let addr = SocketAddr::from(([127, 0, 0, 1], port));
    let mut stream = TcpStream::connect_timeout(&addr, Duration::from_millis(400)).ok()?;
    let _ = stream.set_read_timeout(Some(Duration::from_millis(1500)));
    let request = format!(
        "GET /healthz HTTP/1.0\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n"
    );
    stream.write_all(request.as_bytes()).ok()?;

    let mut body = Vec::new();
    let mut buf = [0u8; 512];
    loop {
        match stream.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                body.extend_from_slice(&buf[..n]);
                if body.len() > 8192 {
                    break;
                }
            }
            Err(_) => break,
        }
    }

    let text = String::from_utf8_lossy(&body);
    let (_, payload) = text.split_once("\r\n\r\n")?;
    let value: serde_json::Value = serde_json::from_str(payload.trim()).ok()?;
    let version = value.get("version")?.as_str()?.trim();
    if version.is_empty() {
        None
    } else {
        Some(version.to_string())
    }
}

/// 本程序自带网关的版本（运行 `workbuddy-gateway version`，结果缓存）。
///
/// 不写死在壳里：版本号在打包时才确定，硬编码会在第一次升版本时漂移，
/// 而这正是本函数要防的那类错误。
fn bundled_gateway_version(bin: &Path) -> Option<String> {
    static CACHE: OnceLock<Option<String>> = OnceLock::new();
    CACHE
        .get_or_init(|| {
            let mut cmd = Command::new(bin);
            cmd.arg("version")
                .stdin(Stdio::null())
                .stderr(Stdio::null())
                .stdout(Stdio::piped());
            configure_no_window(&mut cmd);
            let out = cmd.output().ok()?;
            parse_version_line(&String::from_utf8_lossy(&out.stdout))
        })
        .clone()
}

/// 从 `WorkBuddy Gateway v0.13.2` 这样的输出里取出 `0.13.2`。
fn parse_version_line(text: &str) -> Option<String> {
    text.split_whitespace().find_map(|token| {
        let rest = token.strip_prefix('v')?;
        if rest.chars().next().is_some_and(|c| c.is_ascii_digit()) {
            Some(rest.to_string())
        } else {
            None
        }
    })
}

/// 端口上已有网关时，判断它是不是本程序自带的那一份。
///
/// 返回 `Some(说明)` 表示**不要复用**：版本对不上，或者干脆读不出身份。
/// 返回 `None` 表示可以复用（包括无法判定、但两边都说不出话的情形 —— 那种情况
/// 复用不会比拒绝更糟，而拒绝会让一个能用的应用打不开）。
///
/// 只在端口上那个**更旧**时才拦。更新的放行：面板是网关自己内嵌的，连上去看到的是
/// 那一版的界面，功能上自洽；把它挡在门外反而会让「先升级了命令行版、桌面壳还是旧的」
/// 这种正常情况打不开。
///
/// 为什么以版本号为判据而不是 PID / 路径：端口上那个进程可能是别的用户、别的安装
/// 形态启动的，我们没有可靠的途径去问它的路径；而 `/healthz` 一直带着 `version`，
/// 三个平台、所有历史版本都有这个字段。
fn version_mismatch(port: u16, bin: Option<&Path>) -> Option<String> {
    let running = running_gateway_version(port)?;
    // 读不到自带版本（例如绿色版没把网关放在预期位置）时不拦：那时连「谁新谁旧」
    // 都无从判断，硬拦只会把一个本来能用的组合挡在门外。
    let bundled = bin.and_then(bundled_gateway_version)?;
    if !version_is_older(&running, &bundled) {
        return None;
    }
    Some(format!(
        "端口 {port} 上已经有一个网关在运行，但它的版本是 v{running}，\n\
         而本程序自带的是 v{bundled} —— 直接连接会让你继续用旧版本，所以这里停下来。\n\n\
         请先退出那个网关再重新打开本程序：\n\
         · 命令行版安装的：在「任务管理器」结束 workbuddy-gateway.exe，\n\
         或运行 Stop-ScheduledTask -TaskName WorkBuddyGateway\n\
         · 托盘里有旧的 WorkBuddy 图标：从它的菜单里选「退出」\n\n\
         数据目录（Windows 是 %LOCALAPPDATA%\\wb-gateway，macOS / Linux 是 ~/.wb-gateway）\n\
         不受影响，账号与配置都还在。"
    ))
}

/// 判断版本 a 是否比 b 旧。
///
/// 只比较点分数字段，忽略 `-slice8` 之类的后缀：这里要回答的是「是不是装了个旧的」，
/// 不是完整的 semver 排序。任何一边解析不出数字时返回 false（宁可放行）——
/// 开发版（`dev`）连自己的版本号都说不清，拦下来只会让人无从处理。
fn version_is_older(a: &str, b: &str) -> bool {
    let (Some(va), Some(vb)) = (numeric_version(a), numeric_version(b)) else {
        return false;
    };
    // 逐段比较，缺的段按 0 算：1.2 < 1.2.1。
    let len = va.len().max(vb.len());
    for i in 0..len {
        let x = va.get(i).copied().unwrap_or(0);
        let y = vb.get(i).copied().unwrap_or(0);
        if x != y {
            return x < y;
        }
    }
    false
}

/// 把 `0.13.2` / `v0.13.2` / `0.13.2-slice8` 解析成数字段。
fn numeric_version(v: &str) -> Option<Vec<u64>> {
    let core = v.trim().trim_start_matches('v');
    let core = core.split(['-', '+']).next()?;
    let mut out = Vec::new();
    for part in core.split('.') {
        out.push(part.parse::<u64>().ok()?);
    }
    if out.is_empty() {
        None
    } else {
        Some(out)
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
        // 声明「这次是桌面壳拉起的」。网关据此判断自己属于哪种安装形态，
        // 更新时只挑桌面版安装包 —— 否则命令行版用户会被装成桌面版，
        // 机器上出现两份安装、快捷方式互相覆盖。
        .env("WB_GATEWAY_HOST", "desktop")
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
        let base = match api_key {
            Some(key) => crate::api_key::panel_url_with_key(port, key),
            None => panel_url(port),
        };
        // 本窗口没有系统标题栏时告诉面板，让它自绘一条（拖拽区 + 最小化/最大化/关闭）。
        // 标记与 ensure_frameless 用的是同一个事实，因此不会出现「系统标题栏 + 自绘顶栏」。
        // 这里调一次是**兜底**：setup() 里窗口可能还没建好（见 ensure_frameless 的说明）。
        let url = if ensure_frameless(app) { with_host_marker(&base) } else { base };
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

/// 本窗口是否已去掉系统标题栏（进程内只判定一次，见 ensure_frameless）。
static FRAMELESS: AtomicBool = AtomicBool::new(false);
static FRAMELESS_DECIDED: AtomicBool = AtomicBool::new(false);

/// 去掉系统标题栏，返回「现在是不是无边框」。
///
/// 为什么要在两个地方调（`main.rs` 的 setup 与这里的 navigate_to_panel）：
/// `setup()` 里窗口可能**还没建好**（那时 get_webview_window 返回 None，静默跳过，
/// 作者实测就踩到了 —— 现象是窗口照旧带系统标题栏，而面板因为收到的地址里没有
/// 标记也就不画自绘顶栏，两边都没错、结果都不对）。所以 setup 尽量早、导航前兜底，
/// 用「只判定一次」的开关保证幂等。
///
/// 返回的是**真的设置成功**而不是「平台不是 macOS」：失败时窗口留着系统标题栏，
/// 面板就不能再画一条自己的，否则界面上会叠出两条标题栏。
pub fn ensure_frameless(app: &AppHandle) -> bool {
    if FRAMELESS_DECIDED.load(Ordering::Relaxed) {
        return FRAMELESS.load(Ordering::Relaxed);
    }
    #[cfg(target_os = "macos")]
    {
        // macOS 保留原生标签栏：无边框窗口在那边会丢掉边缘拖拽缩放与交通灯按钮，
        // 那是当地用户的基本预期，而这条路径我们无法在开发机上验证。
        let _ = app;
        FRAMELESS_DECIDED.store(true, Ordering::Relaxed);
        false
    }
    #[cfg(not(target_os = "macos"))]
    {
        let Some(win) = app.get_webview_window("main") else {
            // 窗口还没建好：这次先不定，留给下一次调用（导航前那次通常就成功了）。
            return false;
        };
        let ok = win.set_decorations(false).is_ok();
        // 一行排障输出：面板「有没有自绘顶栏」取决于这里的结果，而窗口外观的问题
        // 从界面上很难判断是「壳没去掉装饰」还是「面板没画顶栏」。壳刻意不引日志依赖，
        // 这一行用 std 的 eprintln 就够（Windows 发布版没有控制台，它会自然消失）。
        eprintln!("[壳] 去掉系统标题栏: {}", if ok { "成功" } else { "失败" });
        // 只在**成功**时才认定「已判定」：失败（GTK 拒绝、窗口状态未就绪）留个口子，
        // 下一次调用（导航前那次）可以再试。反过来写会出现「试过一次失败就永远带
        // 系统标题栏」—— 而这正是作者第一版踩到的那个现象。
        if ok {
            FRAMELESS.store(true, Ordering::Relaxed);
            FRAMELESS_DECIDED.store(true, Ordering::Relaxed);
        }
        ok
    }
}

/// 在 URL 的路径与 fragment 之间插入 `?host=desktop`。
///
/// 为什么用 query 而不是 fragment：壳注入的密钥走 fragment，而面板读到密钥后会立刻
/// 把 fragment 抹掉（避免密钥停在地址栏里）—— 标记放那儿会被一起清掉。
fn with_host_marker(url: &str) -> String {
    match url.split_once('#') {
        Some((head, tail)) => format!("{head}?host=desktop#{tail}"),
        None => format!("{url}?host=desktop"),
    }
}

/// 启动流程（在后台线程执行，不阻塞事件循环）：
///   已就绪 → 直接复用；否则拉起 → 轮询就绪 → 导航。
pub fn boot(app: AppHandle, port: u16) {
    let extra_dirs = resource_dirs(&app);
    let bin = locate("workbuddy-gateway.exe", &extra_dirs)
        .or_else(|| locate("workbuddy-gateway", &extra_dirs));

    if probe(port) {
        // 已经有网关在跑（用户先前用 CLI / 安装包启动过，或上一个壳没退干净）。
        // 直接复用而不是报"端口被占用"——后者会让用户面对一个打不开的应用。
        //
        // 但**必须核对版本**：端口上那个可能根本不是本程序自带的这份（典型来源是
        // 命令行安装包注册的登录计划任务，或另一个旧版本的桌面版）。不核对就导航，
        // 用户看到的是「装完新版还是旧版本号」，而面板里的更新提示还在催他升级。
        if let Some(mismatch) = version_mismatch(port, bin.as_deref()) {
            set_error(&app, &mismatch);
            return;
        }
        // 复用时不生成密钥（那个进程用的不是我们生成的），只在**已有密钥文件**时
        // 注入一次：那是我们上次启动时配的，能命中就省掉手动输入，命中不了
        // 面板会自己弹出输入框，没有副作用。
        set_status(&app, "检测到已运行的网关，正在连接…");
        navigate_to_panel(&app, port, existing_api_key().as_deref());
        return;
    }

    let Some(bin) = bin else {
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

#[cfg(test)]
mod tests {
    use super::{numeric_version, parse_version_line, version_is_older, with_host_marker};

    /// 版本行解析：真实输出是 `WorkBuddy Gateway v0.13.2`。
    ///
    /// 解析失败会让「版本不一致」的检查静默退化（读不到就不拦），
    /// 于是这个功能在第一次输出格式变化时悄悄失效 —— 所以锁住它。
    #[test]
    fn parses_version_line() {
        assert_eq!(
            parse_version_line("WorkBuddy Gateway v0.13.2\n").as_deref(),
            Some("0.13.2")
        );
        assert_eq!(parse_version_line("v1.2.3").as_deref(), Some("1.2.3"));
        assert_eq!(parse_version_line("WorkBuddy Gateway dev"), None);
        assert_eq!(parse_version_line(""), None);
    }

    /// 只在端口上那个更旧时才拦；更新的、说不清的都放行。
    #[test]
    fn compares_versions_for_reuse() {
        assert!(version_is_older("0.9.3", "0.13.2"));
        assert!(version_is_older("0.13.1", "0.13.2"));
        assert!(version_is_older("1.2", "1.2.1"));
        assert!(!version_is_older("0.13.2", "0.13.2"));
        assert!(!version_is_older("0.13.2", "0.9.3"));
        assert!(!version_is_older("1.2.1", "1.2"));
        // 后缀不参与比较：0.9.0-slice8 与 0.9.0 视为同一版。
        assert!(!version_is_older("0.9.0-slice8", "0.9.0"));
        assert!(version_is_older("0.9.0-slice8", "0.9.1"));
        // 解析不出数字时一律放行，避免把开发版挡在门外。
        assert!(!version_is_older("dev", "0.13.2"));
        assert!(!version_is_older("0.13.2", "dev"));
    }

    #[test]
    fn parses_numeric_versions() {
        assert_eq!(numeric_version("v0.13.2"), Some(vec![0, 13, 2]));
        assert_eq!(numeric_version("0.13.2-slice8"), Some(vec![0, 13, 2]));
        assert_eq!(numeric_version("dev"), None);
        assert_eq!(numeric_version(""), None);
    }

    /// 标记必须落在路径与 fragment 之间：面板读到密钥后会抹掉 fragment，
    /// 标记若跟着走就丢了（那样顶栏会时有时无）。
    #[test]
    fn host_marker_sits_before_fragment() {
        let url = with_host_marker("http://127.0.0.1:8317/panel/#wb-key=wb-abc");
        assert_eq!(url, "http://127.0.0.1:8317/panel/?host=desktop#wb-key=wb-abc");
        // 密钥仍在 fragment 里（不发给服务器）。
        assert_eq!(url.split_once('#').unwrap().1, "wb-key=wb-abc");
    }

    /// 没有 fragment 时（没配密钥的外部实例）也要能拼。
    #[test]
    fn host_marker_without_fragment() {
        assert_eq!(
            with_host_marker("http://127.0.0.1:8317/panel/"),
            "http://127.0.0.1:8317/panel/?host=desktop"
        );
    }
}

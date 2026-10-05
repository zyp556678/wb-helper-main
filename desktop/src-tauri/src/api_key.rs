//! 桌面版的接入密钥：首次运行生成一次，之后一直复用。
//!
//! ## 为什么桌面版也要配密钥
//!
//! 网关在本机代理的是**付费账号**。不配密钥时它对本机任何进程都是敞开的 ——
//! 别的程序可以直接拿你的账号额度。命令行/容器版默认只监听 127.0.0.1，
//! 风险有限；但桌面版既然要把 Base URL + 密钥交给用户去接别的工具，
//! 那这个密钥就应该是**真的在校验**的，否则用户填进去的是一个不被检查的摆设。
//!
//! ## 为什么生成一次就落盘
//!
//! 用户会把这个密钥填进 Claude Code / Cursor 之类的外部工具。如果每次启动都换，
//! 那些配置就全失效了。所以：有文件就用文件里的，没有才生成。

use std::path::Path;

/// 密钥文件名（放在网关数据目录下，与账号池同一处）。
pub const API_KEY_FILE: &str = "api-key.txt";

/// 读取已有密钥；没有（或为空）就生成一个并落盘。
///
/// 自己负责建目录，**不假设调用方已经建好**：首次安装时数据目录并不存在，
/// 直接 `fs::write` 会拿到 os error 3（系统找不到指定的路径），
/// 表现为「一装好就启动失败」——而开发机上目录早就在了，根本测不出来。
pub fn ensure_api_key(dir: &Path) -> std::io::Result<String> {
    std::fs::create_dir_all(dir)?;
    let path = dir.join(API_KEY_FILE);
    if let Ok(existing) = std::fs::read_to_string(&path) {
        let trimmed = existing.trim();
        if !trimmed.is_empty() {
            return Ok(trimmed.to_string());
        }
    }
    let key = generate();
    std::fs::write(&path, &key)?;
    restrict_permissions(&path);
    Ok(key)
}

/// 生成 `wb-` + 32 位十六进制（128 位熵）。
fn generate() -> String {
    let mut buf = [0u8; 16];
    // 取不到系统随机数时没有合理的降级方案：用时间戳之类当密钥比不设密钥更糟
    // （会给人虚假的安全感）。真取不到的话，这个环境本身也不适合跑这个应用。
    getrandom::fill(&mut buf).expect("读取系统随机数失败");
    let mut out = String::with_capacity(35);
    out.push_str("wb-");
    for byte in buf {
        // 固定两位十六进制，避免出现长度参差的密钥。
        out.push_str(&format!("{byte:02x}"));
    }
    out
}

/// 收紧文件权限。
///
/// Unix 上直接 0600。Windows 上不做特殊处理：数据目录本来就在用户 profile 下，
/// 继承的 ACL 已经把其他非管理员用户挡在外面；再叠一层 ACL 需要额外依赖，
/// 收益不成比例。
fn restrict_permissions(path: &Path) {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600));
    }
    #[cfg(not(unix))]
    {
        let _ = path;
    }
}

/// 把密钥编进面板地址的 **fragment**（`#wb-key=...`）。
///
/// 为什么走 fragment 而不是 query：fragment **不会发给服务器**，因此不会进
/// 网关的访问日志、也不会被反向代理记下来。面板脚本读到后会立刻
/// `history.replaceState` 把它从地址栏和历史里抹掉。
pub fn panel_url_with_key(port: u16, key: &str) -> String {
    format!(
        "http://127.0.0.1:{port}/panel/#wb-key={}",
        url_encode(key)
    )
}

/// 最小化的百分号编码：密钥是我们自己生成的 `[A-Za-z0-9-]`，但这里仍按
/// 通用规则编码，避免以后有人换成含特殊字符的密钥时悄悄出错。
fn url_encode(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for byte in value.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(byte as char)
            }
            _ => out.push_str(&format!("%{byte:02X}")),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_dir(name: &str) -> std::path::PathBuf {
        std::env::temp_dir().join(format!("wb-api-key-{}-{name}", std::process::id()))
    }

    /// 数据目录**不存在**时也要能生成密钥。
    ///
    /// 这条是回归测试：最初的实现直接 `fs::write` 到数据目录，首次安装时
    /// 目录还没建，于是「一装好就启动失败（os error 3）」。
    /// 开发机上目录早就存在，所以这个 bug 在本地完全看不出来。
    #[test]
    fn creates_missing_directory() {
        let base = temp_dir("fresh");
        let _ = std::fs::remove_dir_all(&base);
        // 故意再嵌两层，确认是递归创建而不是只建一层
        let nested = base.join("deep").join("nested");
        assert!(!nested.exists(), "测试前提：目录应当不存在");

        let key = ensure_api_key(&nested).expect("首次运行必须能生成密钥");
        assert!(key.starts_with("wb-"), "密钥应以 wb- 开头，实际 {key}");
        assert_eq!(key.len(), 35, "wb- + 32 位十六进制，实际 {key}");
        assert!(nested.join(API_KEY_FILE).exists(), "密钥文件应当落盘");

        let _ = std::fs::remove_dir_all(&base);
    }

    /// 第二次调用必须返回**同一个**密钥。
    ///
    /// 用户会把它填进别的工具，每次启动换一个会让那些配置全部失效。
    #[test]
    fn reuses_existing_key() {
        let dir = temp_dir("reuse");
        let _ = std::fs::remove_dir_all(&dir);

        let first = ensure_api_key(&dir).expect("首次生成失败");
        let second = ensure_api_key(&dir).expect("第二次读取失败");
        assert_eq!(first, second, "重复调用不应换密钥");

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 空文件（写入过程中断电等）要当作「没有」，重新生成而不是返回空密钥。
    #[test]
    fn regenerates_when_file_is_empty() {
        let dir = temp_dir("empty");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join(API_KEY_FILE), "   \n").unwrap();

        let key = ensure_api_key(&dir).expect("空文件时应重新生成");
        assert!(key.starts_with("wb-") && key.len() == 35, "实际 {key}");

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 生成的密钥要能安全地放进 URL fragment。
    #[test]
    fn panel_url_carries_key_in_fragment() {
        let url = panel_url_with_key(8317, "wb-abc");
        assert_eq!(url, "http://127.0.0.1:8317/panel/#wb-key=wb-abc");
        // fragment 不发给服务器 —— 这正是选它而不是 query 的原因
        assert!(!url.contains("?wb-key="), "密钥不该出现在 query 里");
    }

    /// 两个密钥不能撞（128 位熵，实际撞的概率可以忽略；这条主要防
    /// 「有人把 generate 换成常量或时间戳」这类退化）。
    #[test]
    fn keys_differ_between_runs() {
        let a = generate();
        let b = generate();
        assert_ne!(a, b, "两次生成不应相同");
    }
}

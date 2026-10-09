# 运行时配置

工作目录下的 `config.json`（可复制 `config.example.json`）覆盖运行层字段：

```json
{
  "upstream": {
    "headerTimeoutSeconds": 300,
    "idleTimeoutSeconds": 120,
    "transientRetries": 2
  },
  "debug":    { "enabled": false },
  "models":   { "blocklist": [], "allowlist": [], "accounts": {} },
  "logging":  { "request_archive_enabled": true, "request_retention_days": 7, "request_archive_max_mb": 100 },
  "server":   { "read_timeout": "300s" },
  "schedule": { "include_disabled_in_tasks": false },
  "update":   { "enabled": true, "check_hours": 6, "repo": "zyp556678/wb-helper-main", "include_prerelease": false, "token": "" }
}
```

- `upstream.headerTimeoutSeconds`：发出请求到收到响应头的上限。上游对 3MB+ 大请求排队预处理可能接近 1 分钟，
  默认放宽到 5 分钟兜底。
- `upstream.idleTimeoutSeconds`：流式响应的空闲读超时。持续有数据永不超时，超过该时长无新数据才判卡死并中断。
- `upstream.transientRetries`：瞬时网络错误重试次数，默认 `2`，**显式写 `0` 表示禁用**。
  网关与上游 CDN 边缘节点之间的单条 TCP 连接可能被对端重置、被关闭，或命中已被回收的
  keep-alive 连接——这类错误属于瞬时故障，值得换条新连接重试一次。
  但**只在请求头尚未写出时重试**：一旦请求头写出，上游就可能已经收到并开始生成，
  此时重放会重复生成与重复计费，所以如实报错而不是偷偷重试。客户端主动取消与上游
  返回的 HTTP 错误响应也不重试。
- `models.blocklist` / `allowlist`：**模型**黑白名单，黑名单优先；白名单非空时只放行列表内模型。
- `models.accounts`：**模型专属的账号名单**，控制「这个模型能用哪些凭据文件」，
  与上面的模型黑白名单互不替代。只接受凭据**文件名**（如 `intl-a.json`），不接受路径或通配符；
  同一模型内黑名单优先，白名单为空表示不限制；模型名忽略大小写。请求调度与失败换号都不绕过名单，
  后台价格探测与本机 `/admin/probe` 也会跳过不允许的账号。若某模型配了名单但**没有任何账号符合**，
  请求返回 `403 model_account_disabled` 且不会调用上游。

```json
{
  "models": {
    "accounts": {
      "deepseek-v4.1-flash": { "allowlist": ["intl-a.json", "intl-b.json"], "blocklist": ["intl-b.json"] },
      "hy3": { "blocklist": ["old-account.json"] }
    }
  }
}
```

  上面这段配置下，`deepseek-v4.1-flash` 最终只允许 `intl-a.json`，`hy3` 仅排除 `old-account.json`，
  未配置的模型不受任何限制。`config.json` 在服务启动时读取，修改后需重启网关。
- `server.read_timeout`：入站请求读取（含 body 上传）的总时长上限，默认 `300s`；`"0"` = 不限制；
  负值或不可解析的写法会**拒绝启动**（静默钳 0 等于把保护悄悄关掉）。大上下文 / 文件块经反代链
  慢速上传被掐成 `400 read body: ... i/o timeout` 时调大它。属装配期字段：**改动需重启进程**。
- `schedule.include_disabled_in_tasks`：保号类四任务（签到 / 活跃上报 / 保活 / 余额刷新）
  是否覆盖**已禁用**账号，默认 `false`（禁用即跳过，与选号过滤一致）。打开后禁用号照常保号、
  依旧不参与选号、也不会被自动解冻 —— 适合「一次只放开一个账号、用禁用做流量开关」的轮换养号用法。
  热生效，无需重启。详见 [PROGRESS.md](PROGRESS.md) 的「任务体系」一节。
- `logging.request_archive_enabled`：请求级 **JSONL 归档**开关，默认 `true`。
  对话类端点（`/v1/chat/completions`、`/v1/responses`）每次请求记一条**脱敏**元数据
  （时间、请求 ID、账号「昵称(uid8)」标签、模型、状态码、结局、耗时、TTFB、重试次数、
  token、消耗），落在工作目录的 `request-logs/requests-YYYY-MM-DD.jsonl`。
  **绝不写提示词、响应正文、Authorization 或完整 UID。**
  归档走有界队列，**队列满时丢弃并计数**（`archive.dropped_writes` 可查），
  不允许日志写盘拖住模型请求。按天 + 按大小（单文件 16 MiB）轮转。
  进程内指标（最近 100 条 + 完成/成功/失败/成功率/平均耗时）**始终启用**，与本开关无关。
- `logging.request_retention_days`：归档保留天数，默认 `7`。
- `logging.request_archive_max_mb`：归档总容量上限（MiB），默认 `100`；超出后从最旧的开始删。
- 查看：面板 `GET /panel/api/request-metrics`（进程内指标 + 最近 100 条 + 归档状态）、
  `GET /panel/api/request-logs?limit=&outcome=&account=&model=`（从归档读，limit 默认 200、最大 1000）。
  响应头 `X-Request-Id` 与记录里的 `request_id` 一致，便于和上游日志对齐。
- `update.enabled`：是否在后台周期检查新版本，默认 `true`。热生效（面板「配置 → 检查更新」
  的「自动检查」开关写的就是它）。默认开是因为代价极低（每 6 小时一次只读 GET），
  而默认关的后果是装了旧版的人永远不会知道有新版本。
- `update.check_hours`：自动检查间隔（小时），默认 `6`，最小 `1`。
- `update.repo`：更新源仓库（`owner/name`），默认 `zyp556678/wb-helper-main`（Release 只发在这里）。
  fork 出去自己发版的用户改成自己的仓库即可。
- `update.include_prerelease`：预发布版本是否参与比较，默认 `false`
  （GitHub 的 prerelease 标记，以及 `v0.9.0-slice8` 这类带后缀的 tag 都算预发布）。
- `update.token`：读取**私有**仓库 Release 的只读令牌（细粒度 PAT，`contents:read` 即可）。
  留空时回落到环境变量 `WB_UPDATE_TOKEN`（容器部署不必把令牌写进 `config.json`）。
  未认证访问私有仓库的 Release 接口一律返回 404，因此**没配令牌时会如实报「需要令牌」**，
  而不是报「已是最新」—— 把「查不到」说成「没有新版本」会让人永远等不到更新。

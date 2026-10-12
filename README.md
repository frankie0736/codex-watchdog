# codex-watchdog

一个常驻终端的 TUI，用来看着本机的 Codex 会话（CLI / VS Code / ChatGPT App 里的 Codex）。

当会话因为 `Selected model is at capacity. Please try a different model.` 失败停下时，它会在限流规则允许的范围内，
自动执行：

```sh
codex queue --thread <thread-id> --message 继续
```

让会话接着跑，不用你守在电脑前一个个手动点“继续”。

> 它只做这一件事：给**因容量错误失败**、且**仍然打开着**的会话发一条“继续”。
> 不删除、不归档、不重启会话，也不改模型。

## 环境要求

- macOS（依赖 `lsof` 判断会话是否仍打开、`pbcopy` 复制 thread ID）
- Codex CLI，且支持 `codex queue` 子命令（开发时用的是 `0.162.x`）
- 从源码构建需要 Go 1.26+

## 安装

```sh
go install github.com/frankie0736/codex-watchdog/cmd/codex-watchdog@latest
```

或者克隆后：

```sh
make install    # 先跑测试，再安装到 ~/.local/bin/codex-watchdog
```

## 使用

**第一次建议先用 dry-run 观察一段时间**，确认它会对哪些会话动手：

```sh
codex-watchdog --dry-run       # 只记录“本应发送”，不执行 codex queue
```

确认没问题后：

```sh
codex-watchdog                 # 启动 TUI（按配置决定是否真实发送，默认真实发送）
```

其它参数：

| 参数 | 说明 |
|---|---|
| `--dry-run` | 强制 dry-run：只记录决策，从不执行 `codex queue` |
| `--live` | 强制真实发送（覆盖配置里的 `dry_run = true`），与 `--dry-run` 互斥 |
| `--once` | 只读轮询一次，向 stdout 输出 JSON 快照后退出，从不发送 |
| `--thread <id>` | 只对这一个 thread 执行发送，其它会话只显示 |
| `--print-config` | 打印生效配置和发现的 Codex home 后退出 |
| `--config <path>` | 指定配置文件（默认 `~/.config/codex-watchdog/config.toml`） |

### 按键

| 键 | 作用 |
|---|---|
| `q` | 退出（只停止监控，不影响任何 Codex 会话） |
| `p` | 暂停 / 恢复自动发送 |
| `d` | 切换 dry-run；从 dry-run 切到真实发送需要再按 `y` 确认 |
| `r` | 立即刷新 |
| `a` | 显示全部会话 / 只显示需要关注的 |
| `Enter` | 查看选中会话详情 |
| `c` | 复制 thread ID |
| `l` | 查看处理记录 |
| `?` | 状态说明 |

### 状态

| 状态 | 含义 |
|---|---|
| `ACTIVE` | 最新 turn 正在运行（在 `active_max_age_seconds` 内） |
| `ORPHANED` | 显示为 inProgress 但太旧，进程大概率已退出（按 `a` 才显示） |
| `WAIT_GRACE` | 容量错误，等待 grace period |
| `COOLDOWN` | 同一会话冷却中 |
| `RATE_LIMITED` | 达到全局每分钟上限 |
| `BACKLOG` | codex 队列里还有未消费的消息，不再叠加 |
| `QUEUEING` | 正在执行 `codex queue` |
| `QUEUED` | 已入队，等待新 turn 出现 |
| `QUEUE_FAILED` | 执行失败，等待重试 |
| `NEEDS_ATTENTION` | 达到最大尝试次数，需要人工处理 |
| `DRY_RUN` | dry-run 模式下记录为“本应发送” |
| `RECOVERED` | 发送后出现了新 turn，会话已恢复 |
| `EXCLUDED` | 容量错误，但来源（如 exec / subagent）或归档状态不允许自动处理 |
| `DETACHED` | 容量错误，但会话已经没有 codex 进程加载，不发送 |
| `SKIPPED` | 启动前就已存在的错误，且配置为不处理 |

## 配置

配置文件不存在时使用内置默认值。需要修改时：

```sh
mkdir -p ~/.config/codex-watchdog
cp config.example.toml ~/.config/codex-watchdog/config.toml
```

各字段说明见 [`config.example.toml`](config.example.toml)。未知字段会直接报错，避免拼错了却悄悄不生效。

几个最常调的默认值：

| 字段 | 默认 | 说明 |
|---|---|---|
| `capacity_error_grace_seconds` | 30 | 错误出现后至少等这么久再处理 |
| `capacity_error_max_age_seconds` | 3600 | 更旧的容量错误视为 `STALE`，不处理 |
| `per_thread_cooldown_seconds` | 300 | 同一会话两次发送的最小间隔 |
| `global_max_sends_per_minute` | 5 | 所有会话合计每分钟上限 |
| `max_attempts_per_turn` | 3 | 同一个失败 turn 最多尝试几次 |
| `allowed_sources` | `["cli", "vscode"]` | ChatGPT App 的 Codex 会话 source 也是 `vscode` |
| `message` | `继续` | 发送的内容 |

默认会自动发现 `~/.codex*` 下含 `state_*.sqlite` 的目录（例如 `~/.codex` 和 `~/.codex-sub`），
每个会话用它自己所属的 `CODEX_HOME` 执行 `codex queue`。

## 工作原理与安全边界

- **只读 Codex 数据**：以 `mode=ro` + `query_only` 打开 Codex 的 SQLite，从不写入。
- **自有状态隔离**：发送记录存在 `~/.local/state/codex-watchdog/state.db`；
  结构化 JSON 日志写到同目录的 `watchdog.log`，每次运行带 `run_id`。
- **不重复发送**：去重键是 `(home, thread, 失败的 turn, 第几次尝试)`。
  **先落库再执行** `codex queue`，进程崩溃后宁可少发一次也不会重发。
- **只给仍打开着的会话发**：用 `lsof -c codex` 检查该会话的 rollout 文件是否被 codex 进程持有；
  已经关掉的会话显示 `DETACHED`。检测失败时一律不发送（fail closed）。
- **不叠加消息**：如果 Codex 自己的 `queue_*.sqlite` 里该会话还有没被消费的消息，就不再追加。
- **决策顺序**：启动策略 → 已有尝试（等待送达 / 失败重试 / 次数上限）→ grace → 单会话 cooldown →
  队列积压 → 全局每分钟上限。
- `codex queue` 以参数数组执行，不经过 shell。
- dry-run 与真实发送分开计数，dry-run 不消耗真实发送配额。

## 已验证 / 未验证

在 codex-cli 0.162 上实测（2026-10-12）：

- ✅ CLI TUI 中打开着的会话：`codex queue` 后约 0.1–0.2s 入队，随即出现新的 inProgress turn。
- ✅ `~/.codex` 与 `~/.codex-sub` 的 `CODEX_HOME` 路由正确。
- ⚠️ 没有客户端挂着的会话：消息会留在 `queue_*.sqlite`，不产生新 turn，watchdog 显示 `BACKLOG` 且不叠加。
  重新打开该会话时是否会消费这条消息，尚未验证。
- ❓ ChatGPT App 中的 Codex 会话：尚未端到端验证。

本工具依赖 Codex 本地 SQLite 的表结构，这不是公开接口，Codex 升级后可能失效。

## 开发

```sh
go test -race ./...
```

`internal/watcher` 的测试使用真实 SQLite（WAL 模式，独立写连接）+ 假 sender + 可控时钟，覆盖：
100 次轮询只发 1 次、重启后不重发、出现新 turn 标记 `RECOVERED`、重试上限、失败重试、
队列积压阻止叠加、全局限流、dry-run 不发送且不占配额、exec/subagent/归档/过期/暂停不发送。

设计背景见 [`docs/需求与调研记录.md`](docs/需求与调研记录.md) 和 [`docs/执行计划.md`](docs/执行计划.md)。

## License

[MIT](LICENSE)

# 本地 Claude 终端桥接使用说明

本文档说明 cc-connect 的本地 Claude 终端桥接功能：在电脑端启动一个真实可见的 Claude Code TUI，并通过飞书/Lark 远程接入同一个终端，实现飞书与本地 CLI 双向同步。

适用版本：cc-connect 1.1.1 及以上。

## 功能概览

本功能解决的是“远程控制本地 Claude CLI 终端”的场景：

- 电脑端本地可以看到并直接操作 Claude Code TUI。
- 飞书可以通过 `/terminal attach <id>` 接入这个本地终端。
- 飞书发送的普通消息会进入本地 Claude TUI。
- 本地电脑端直接输入 Claude TUI 的问题，结果也会回传到飞书。
- 回复默认使用 `screenshot-progress` 模式：工具执行时发过程截图，终端静止后发最终截图。
- 终端输出超过一屏时，会按阅读顺序发送多张 PNG。
- `/terminal screenshot latest` 只截最近/当前这一轮，不把旧历史一起发回。

## 启动前提

先正常启动 cc-connect 服务，并确保飞书/Lark 平台已经可用。

然后在要操作的项目目录或任意目录启动本地 Claude 终端桥接。

## 启动本地 Claude 终端

推荐使用绝对路径指定项目目录：

```bash
cc-connect terminal claude --project ClaudeCode --workdir "E:\\MyData\\XILINX\\RX_EM\\PLAN\\ZYNQ规划" --data-dir "E:\\MyData\\ClaudeCode\\cc-connect\\.cc-connect"
```

参数说明：

| 参数 | 说明 |
|------|------|
| `--project` | cc-connect 中对应的项目名，用来找到 daemon 里的项目 Engine。 |
| `--workdir` | Claude Code TUI 启动所在目录，也就是你要远程调试/操作的项目目录。 |
| `--data-dir` | cc-connect 数据目录，用来找到本地 API/socket 和附件目录。 |
| `--resume` | 可选。传入 Claude Code 的 session id，用于恢复已有 Claude 会话。 |
| `--no-report-local-input` | 可选。关闭本地 CLI 输入回传飞书，通常不要加。 |

启动后终端会打印类似：

```text
Terminal registered: term_000001
```

记住这个 `term_000001`，飞书端 attach 时要用。

## 飞书端接入终端

在飞书里发送：

```text
/terminal attach term_000001
```

接入成功后：

- 飞书里直接发普通文字，会进入本地 Claude TUI。
- 本地 Claude TUI 的输出会回传飞书。
- 电脑端直接在 Claude TUI 输入内容，结果也会回传飞书。

## 常用飞书命令

| 命令 | 作用 |
|------|------|
| `/terminal list` | 查看当前可接入的本地终端列表。 |
| `/terminal attach <id>` | 接入指定终端，例如 `/terminal attach term_000001`。 |
| `/terminal detach` | 断开飞书到终端的直接输入通道，但保留本地 CLI 输出回传目标。 |
| `/terminal send <text>` | 手动向已接入终端发送一段文字。通常 attach 后可以直接发普通消息，不必每次使用该命令。 |
| `/terminal mode` | 查看当前终端回复模式。 |
| `/terminal mode screenshot-progress` | 设置为过程截图 + 最终截图模式。当前版本只保留这个模式。 |
| `/terminal screenshot` | 发送当前完整终端窗口/历史的截图。 |
| `/terminal screenshot latest` | 只发送最近/当前这一轮问答的截图。 |
| `/terminal stop` | 请求停止本地终端。 |

## 回复模式说明

当前版本只保留一个推荐模式：

```text
/terminal mode screenshot-progress
```

这个模式的行为是：

1. Claude TUI 执行工具、搜索、命令等阶段，会发送过程截图。
2. 终端继续动态刷新时不会立刻发最终结果。
3. 等终端静止一段时间后，发送最终截图。
4. 如果最终结果超过一屏，会发送多张 PNG。
5. 动态状态文字会被过滤，例如 Roosting、Context、Web Search 状态行、Did 0 searches 等。

## `/terminal screenshot` 和 `/terminal screenshot latest` 的区别

### `/terminal screenshot`

发送当前终端完整可见状态和已保留的滚动历史。

适合：

- 想看当前本地终端整体画面。
- 想确认终端历史上下文。
- 想手动补发当前窗口截图。

### `/terminal screenshot latest`

只发送最近或当前这一轮问答的截图。

适合：

- 不想把旧问答历史重新发一遍。
- 只想看刚才这次 Claude 回复。
- 本地 CLI 输入后，只想补发本轮结果。

## 本地 CLI 输入如何回传飞书

启动 `cc-connect terminal claude` 后，cc-connect 会监听本地键盘输入是否提交。

当你在电脑端 Claude TUI 里直接输入并按回车后：

1. cc-connect 向 daemon 上报一个安全的 `<local-input>` 信号。
2. daemon 为这次本地输入创建一轮 terminal turn。
3. 后续 Claude TUI 输出会按当前 terminal mode 回传飞书。
4. 在 `screenshot-progress` 模式下，飞书收到的是过程截图和最终截图。

注意：cc-connect 不会把你本地键盘输入的原始内容原样发到飞书，避免泄漏密钥或大段粘贴内容。

## attach、detach、重新 attach 的行为

### attach

```text
/terminal attach term_000001
```

建立飞书到本地终端的输入通道，并记录本地 CLI 输出要回传到哪个飞书会话。

### detach

```text
/terminal detach
```

断开飞书普通消息到本地终端的输入通道。

但本地 CLI 输出回传目标会保留：也就是说，detach 后你如果还在电脑端 Claude TUI 输入，飞书仍可以收到这轮本地输入的结果。

### 重新 attach 到另一个终端

```text
/terminal attach term_000002
```

重新 attach 后，本地 CLI 输出会使用新的终端和新的飞书上下文，不会继续发到旧 terminal 的会话里。

## 推荐使用流程

1. 启动 cc-connect daemon。
2. 在电脑端启动本地 Claude 终端：

```bash
cc-connect terminal claude --project ClaudeCode --workdir "你的项目路径" --data-dir "你的 cc-connect 数据目录"
```

3. 在飞书中查看终端：

```text
/terminal list
```

4. 接入终端：

```text
/terminal attach term_000001
```

5. 确认模式：

```text
/terminal mode
```

6. 如果需要，设置模式：

```text
/terminal mode screenshot-progress
```

7. 之后直接在飞书发送普通消息即可远程操作本地 Claude TUI。

8. 如果在电脑端直接输入 Claude TUI，飞书也会收到这轮输出。

## 常见问题

### 飞书发消息是否每次都要 `/terminal send`？

不用。

attach 成功后，飞书普通消息会自动进入本地终端。`/terminal send <text>` 只是保留的手动发送命令。

### 为什么只保留 `screenshot-progress` 模式？

终端 TUI 会动态刷新，纯文本模式容易丢尾部结果或混入状态噪声。`screenshot-progress` 更接近真实终端画面，也更适合远程调试。

### 为什么偶尔之前会看到 `Web Search(...)` 或 `Did 0 searches`？

这些是 Claude TUI 的工具状态行，不是最终回答。1.1.1 后已把这类工具块纳入截图流程，避免作为飞书文本消息单独蹦出来。

### `/terminal screenshot latest` 还是看到旧内容怎么办？

先确认最近是否真的产生了新的 terminal turn。正常情况下：

- 飞书发送普通消息会创建一轮。
- 本地 CLI 输入并回车会创建一轮。
- 最新截图会优先使用当前/最近一轮，而不是完整历史。

### 图片没有发出来怎么办？

检查：

1. 当前平台是否支持图片发送。
2. 飞书应用是否有发送图片/文件权限。
3. cc-connect 服务日志里是否有 `terminal screenshot send failed`。
4. 是否正确 attach 到目标 terminal。

### 本地输入不想回传飞书怎么办？

启动本地终端时加：

```bash
cc-connect terminal claude --no-report-local-input ...
```

通常不建议关闭，否则本地电脑端直接输入时，飞书不会收到结果。

## 安全说明

- 本地键盘输入不会被原样发送到飞书，只会上报安全信号。
- 飞书收到的是 Claude TUI 输出截图或过滤后的结果。
- 不建议在远程群聊里 attach 到含敏感项目的本地终端。
- 如果终端里显示密钥、token、私有路径等内容，截图会如实显示，请注意使用场景。

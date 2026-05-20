# 本地 Claude 终端桥接使用说明

本文说明如何用 cc-connect 在飞书/Lark 中远程控制电脑端正在运行的 Claude Code TUI，并让本地 CLI 与飞书保持双向同步。

适用版本：cc-connect 1.1.1 及以上。

## 1. 功能概览

本地终端桥接用于把一个真实可见的 Claude Code 终端注册到 cc-connect：

- 电脑端能看到完整 Claude TUI。
- 飞书可以 attach 到这个终端并发送指令。
- 飞书消息会进入同一个本地 Claude TUI。
- 本地电脑端直接输入 Claude TUI 时，结果也会回传飞书。
- 终端回复默认使用 `screenshot-progress`：工具执行阶段发过程截图，终端静止后发最终截图。
- 终端输出超过一屏时，会按顺序发送多张 PNG。

## 2. 启动本地 Claude 终端

先启动 cc-connect 服务，然后在要调试的项目目录运行：

```bash
cc-connect terminal claude --project ClaudeCode
```

也可以从任意目录指定工作目录：

```bash
cc-connect terminal claude --project ClaudeCode --workdir "E:\\MyData\\XILINX\\RX_EM\\PLAN\\ZYNQ规划"
```

如果你的服务使用单独数据目录：

```bash
cc-connect terminal claude --project ClaudeCode --workdir "E:\\MyData\\XILINX\\RX_EM\\PLAN\\ZYNQ规划" --data-dir "E:\\MyData\\ClaudeCode\\cc-connect\\.cc-connect"
```

启动成功后，终端会打印一个 ID，例如：

```text
term_000001
```

这个 ID 就是飞书端 attach 用的终端 ID。

## 3. 飞书接入本地终端

在飞书中先查看终端列表：

```text
/terminal list
```

接入指定终端：

```text
/terminal attach term_000001
```

attach 后，飞书里直接发送普通消息即可进入本地 Claude TUI，不需要每次写 `/terminal send`。

如果你想手动发送一段内容，也可以用：

```text
/terminal send 帮我检查当前项目编译错误
```

## 4. 终端截图模式

当前版本只保留一个终端回复模式：

```text
/terminal mode screenshot-progress
```

查看当前模式：

```text
/terminal mode
```

`screenshot-progress` 的行为：

1. 工具执行阶段可能发送过程截图。
2. 等终端没有动态刷新后，再发送最终截图。
3. 如果输出超过一屏，会按阅读顺序发送多张 PNG。
4. 会过滤 Claude TUI 的动态噪声，例如 Roosting、Context、计时、Web Search 状态行等。

## 5. 两种截图指令的区别

发送当前完整终端窗口/历史截图：

```text
/terminal screenshot
```

只发送最近或当前这一轮问答截图：

```text
/terminal screenshot latest
```

区别：

| 指令 | 范围 | 适合场景 |
|---|---|---|
| `/terminal screenshot` | 当前完整终端屏幕和可用滚动历史 | 想看当前终端整体状态 |
| `/terminal screenshot latest` | 最新/当前这一轮问答 | 不想把历史问答重复发到飞书 |

## 6. detach、重新 attach 与本地输入回传

断开飞书到本地终端的直接输入：

```text
/terminal detach
```

`detach` 后：

- 飞书普通消息不再直接进入本地终端。
- 电脑端本地 CLI 继续输入时，结果仍会回传到最近 attach 过的飞书会话。

重新接入另一个终端：

```text
/terminal attach term_000002
```

重新 attach 后，本地 CLI 输入结果会回传到新的 attach 目标。

## 7. 本地窗口拖拽、跨屏和分辨率变化

当 Windows Terminal 或本地终端窗口被拖拽、调整大小、跨显示器移动时，cc-connect 会持续同步本地窗口尺寸到 PTY/ConPTY，避免 Claude TUI 仍按旧宽度绘制导致分隔线重复、错位、残影或截图格式混乱。

如果仍看到画面错乱，可以先尝试：

1. 等待 Claude TUI 静止后再操作。
2. 在本地终端按一次 Enter 触发重绘。
3. 重新 `/terminal screenshot latest`。
4. 如仍异常，重启 `cc-connect terminal claude` 这一侧终端。

## 8. 查看当前桥接的是哪个会话

普通 cc-connect Agent 会话：

```text
/current
/list
```

本地 Claude 终端桥接：

```text
/terminal list
```

当前飞书接入哪个本地终端，取决于最近一次：

```text
/terminal attach <terminal_id>
```

## 9. 修改会话名字

普通 cc-connect Agent 会话可以改名：

```text
/name 我的项目调试会话
```

或者：

```text
/rename 我的项目调试会话
```

修改 `/list` 中某个序号的会话名：

```text
/name 2 ZYNQ规划调试
```

注意：`/name` 修改的是普通 Agent 会话名，不是 `term_000001` 这种本地终端 ID。当前本地终端 ID 暂不支持单独改名，建议通过 `/terminal list` 中的 project 和 workdir 区分。

## 10. 常用飞书指令速查

### 会话管理

```text
/new [名称]
/list
/switch <序号或会话ID>
/current
/name <新名字>
/name <序号> <新名字>
/delete <序号或会话ID>
/history
```

### 本地终端桥接

```text
/terminal list
/terminal attach <id>
/terminal detach
/terminal send <内容>
/terminal mode
/terminal mode screenshot-progress
/terminal screenshot
/terminal screenshot latest
/terminal stop
```

### 模型与运行控制

```text
/model
/mode
/reasoning
/quiet
/stop
/compress
```

### 工作目录和调试

```text
/dir
/dir <路径>
/cd <路径>
/diff
/search <关键词>
/show <文件或引用>
/shell <命令>
```

### 管理与诊断

```text
/help
/commands
/version
/whoami
/lang
/config
/doctor
/restart
/alias
/cron
/heartbeat
/skills
/memory
```

## 11. 推荐日常流程

1. 在电脑端项目目录启动：

```bash
cc-connect terminal claude --project ClaudeCode --workdir "你的项目路径"
```

2. 在飞书查看并接入：

```text
/terminal list
/terminal attach term_000001
```

3. 确认模式：

```text
/terminal mode
```

4. 直接在飞书发普通消息控制本地 Claude TUI。

5. 需要看最新结果时：

```text
/terminal screenshot latest
```

6. 需要看完整窗口历史时：

```text
/terminal screenshot
```

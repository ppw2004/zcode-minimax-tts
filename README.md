# ZCode + MiniMax TTS

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://golang.org)

**[ybouhjira/claude-code-tts](https://github.com/ybouhjira/claude-code-tts) 的深度改造 fork**：在原插件的 OpenAI/MiniMax 双引擎 MCP 语音基础上，为 [ZCode](https://zcode.ai) 重新设计了**常驻守护进程（ttsd）自动语音方案**——ZCode 的回复（包括工具调用之间的中间输出）全程自动朗读，且完全不阻塞 ZCode。

> 原仓库的 MCP 服务器（`tts-server`）与命令行工具（`speak-text`）在本 fork 中保持可用，架构说明见文末。

## 本 fork 新增了什么

| 能力 | 说明 |
|------|------|
| **中间输出也朗读** | ttsd 直接 tail ZCode 的 rollout 日志（`model-io-*.jsonl`），每次模型调用的 `response.text` 落盘 400ms 内即被拾取朗读——工具调用之间的每段进度播报都能听到 |
| **完全零阻塞** | ZCode 的 process 型钩子会内联等待整个进程树（Windows job object），"后台启动子进程"无效。本方案让钩子只做一个 <300ms 的 HTTP ping，语音由计划任务拉起的守护进程独立完成，ZCode 关闭也不影响播放 |
| **有序队列 + 片段停顿** | 播放严格按文本产生顺序串行；当前片段播放时，后续片段已在并行预取合成；片段之间固定停顿 500ms（`TTS_GAP_MS` 可调） |
| **绝不重复** | 每段语音的 requestId 在播放完成后持久化落盘；守护进程无论重启多少次，播过的段落永不重播；崩溃补播窗口 10 分钟——宕机期间漏掉的段落复活后补上 |
| **无字数限制** | 每段全文朗读（`TTS_MAX_CHARS` 默认 0=不限） |
| **零窗口** | ttsd 以 GUI 子系统编译（`-H windowsgui`），播放子进程 `CREATE_NO_WINDOW`，桌面上没有任何可误关的控制台 |
| **三层自启** | SessionStart/UserPromptSubmit 钩子自检 + 登录自启 + 计划任务拉活；单实例守卫防双开 |

## 架构

```
ZCode 钩子（秒退）                ttsd.exe 常驻守护进程（计划任务拉起，零窗口）
SessionStart ──ping──┐            ┌─ Watcher: tail rollout/model-io-*.jsonl
UserPromptSubmit ────┼──HTTP──→   │   新行 → 清洗 → 入队（已播 requestId 过滤）
（死了就 schtasks    │ 127.0.0.1  ├─ Pipeline: 并行预取合成 + 严格 FIFO 播放
  拉活，三层保障）    │  :9750     │   片段间停顿 TTS_GAP_MS（默认 500ms）
                     └────────────┴─ 持久化: 播放完成即记录，重启永不重播
```

## 安装（Windows / ZCode）

```powershell
# 1. 编译（GUI 子系统，无窗口）
go build -ldflags "-H windowsgui" -o ttsd.exe .\cmd\ttsd

# 2. 部署文件（模板见 deploy/ 目录，替换 <YOUR_MINIMAX_KEY>）
Copy-Item ttsd.exe        ~\.zcode\hooks\
Copy-Item deploy\*        ~\.zcode\hooks\     # ttsd-start.ps1 / ttsd-ensure.ps1 / zcode-ttsd.vbs

# 3. 注册计划任务（手动型，仅按需触发；守护进程脱离任务树，不受 72h 强杀限制）
schtasks /create /f /tn "ZCodeTTSWatcher" /tr "powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File C:\Users\<you>\.zcode\hooks\ttsd-start.ps1" /sc once /st 23:59

# 4. 登录自启（可选的第三层保障）
Copy-Item ~\.zcode\hooks\zcode-ttsd.vbs "$env:APPDATA\Microsoft\Windows\Start Menu\Programs\Startup\"

# 5. ZCode 钩子（~/.zcode/cli/config.json 的 hooks.events，均挂 ttsd-ensure.ps1，秒退）
#    SessionStart + UserPromptSubmit，hooks.enabled: true

# 6. 拉起并验证
schtasks /run /tn "ZCodeTTSWatcher"
curl http://127.0.0.1:9750/status
```

> ⚠️ 写 `~/.zcode/cli/config.json` 必须无 BOM（UTF-8 BOM 会导致 ZCode 解析失败并丢弃整个用户配置）。

## 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `TTS_PROVIDER` | 自动探测 | `minimax` / `openai` |
| `MINIMAX_API_KEY` | — | MiniMax 密钥（t2a_v2 无需 GroupId） |
| `MINIMAX_BASE_URL` | `api.minimaxi.com` | 或 `api.minimax.cn`（`.io` 域名报 2049 无效密钥） |
| `MINIMAX_MODEL` | `speech-02-hd` | 或 `speech-02-turbo` |
| `MINIMAX_VOICE` | `female-shaonv` | 全部可用音色见原 README 语音章节 |
| `TTS_PORT` | `9750` | HTTP API 端口 |
| `TTS_GAP_MS` | `500` | 片段间停顿 |
| `TTS_MAX_CHARS` | `0`（不限） | 每段朗读字数上限，0 = 全文 |
| `TTS_ROLLOUT_DIR` | `~/.zcode/cli/rollout` | 监视的日志目录 |
| `TTS_POLL_MS` | `400` | 目录轮询间隔 |
| `TTS_SPOKEN_FILE` | exe 同目录 `ttsd-spoken.jsonl` | 已播记录持久化文件 |

## HTTP API

| 端点 | 说明 |
|------|------|
| `GET /ping` | 存活探测（钩子自检用） |
| `GET /status` | 队列/播放/计数快照（播放期间也秒回） |
| `POST /say` | 手动点播 `{"text":"...","voice":"..."}` |
| `POST /clear` | 清空未播队列（按用户意图视为已处理，不会复活） |
| `POST /skip` | 跳过当前正在播的片段 |

## 文本清洗规则

代码块 → “，代码块略过。”；URL → “，链接略过。”；去除 Markdown 装饰符号；`reasoningText`（思考内容）不朗读；全文朗读不限字数。

## 上游原有能力（保持兼容）

- `tts-server`：MCP 服务器（speak / tts_status / tts_pause / tts_resume / tts_clear）
- `speak-text`：一次性命令行朗读
- OpenAI（tts-1）与 MiniMax（t2a_v2，请求 PCM 并封装 WAV 以兼容 Windows SoundPlayer）双引擎
- 跨平台播放：macOS afplay / Linux mpv 等 / Windows PowerShell

## 排障

- 看日志：`~/.claude/logs/tts-server.log`（按 `queued segment` / `completed` / `adopted` 关键字过滤）
- 有重复？按段落文本在日志里分组，计数 >1 即守护进程侧问题；ZCode 日志里出现 `config.Stop.0.0` 则是旧 Stop 朗读钩子未清干净
- 没声音？`schtasks /run /tn ZCodeTTSWatcher` 后 `curl http://127.0.0.1:9750/ping` 验证

## License

MIT（继承上游）。

## Credits

- 上游项目：[ybouhjira/claude-code-tts](https://github.com/ybouhjira/claude-code-tts)
- [mcp-go](https://github.com/mark3labs/mcp-go)
- MiniMax TTS（t2a_v2）

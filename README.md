# autoclaw2api

把 [AutoClaw](https://autoclaw.z.ai/) 云端沙箱封装成 **OpenAI 兼容 API** 的聚合网关，支持**多账号池 + 积分加权轮换 + 冷却/禁用状态机 + token 自动续期**。参考项目：[@Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 与 [@Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api)。

> 本项目仅供学习与技术交流。使用前请阅读并遵守 AutoClaw 的 [服务条款](https://www.z.ai/agreement)、[隐私政策](https://www.z.ai/privacy) 及各适用地区法律法规。请勿用于对外收费转售、暴力破解、账号共享滥用等违反条款的行为；请合理控制并发，避免造成平台负载压力。

---

## 特性

- **OpenAI 兼容**：`/v1/chat/completions`（支持 `stream`）+ `/v1/models` + `/status` + `/healthz`
- **多账号池**：自动扫描 `auths/autoclaw-*.json`，积分 **Top5 加权随机** 挑号，避免并发撞号窗口
- **双通道**：默认从沙箱网关 OpenAI 兼容端点转发；上游未启用该端点时**自动回退**到网关 relay bridge（SSE 事件流 → OpenAI 流式块），无需改配置
- **状态机**：余额不足→长冷却、429→短冷却、连续错误→中冷却、token 失效→禁用，全部落盘 `data/state.json`，人工重登后自动/手动恢复
- **自动维护**：后台调度每 30 分钟做一次 token 预刷新 + 积分刷新（恢复的账号自动解冻）+ 沙箱确保/唤醒
- **同进程内 token 刷新用锁串行化**，杜绝并发写回半更新
- 模块内所有请求都带 AutoClaw 签名头（`X-Auth-Sign`=MD5）

## 技术栈

Go 1.22+（纯标准库，无第三方依赖）。

## 目录结构

```
.
├── cmd/
│   ├── server/        # 主服务（HTTP 网关 + 调度器）
│   ├── login/         # 登录工具（短信验证码换 token → 落盘凭证）
│   ├── credit/        # 积分/状态查询工具
│   └── maintain/      # 手动执行一轮维护（刷新+续期+沙箱）
├── internal/
│   ├── auth/          # 凭证加载/解析/原子写回 + token 到期判断
│   ├── upstream/      # userapi 签名客户端 + 沙箱网关 OpenAI/relay/SSE 转换
│   ├── pool/          # 账号池：权重轮换、冷却/禁用状态机、state.json 持久化
│   ├── scheduler/     # 后台维护（token/积分/沙箱）
│   └── server/        # HTTP handler + OpenAI 错误格式
├── config.json        # 服务配置
├── auths/             # 账号凭证（*gitignored*）
├── data/              # 池状态文件（*gitignored*）
├── Dockerfile
├── login.sh / credit.sh / maintain.sh
└── docs/
```

## 快速开始

### 1. 构建

```bash
./build.sh          # 会执行 vet + test + 构建 ./bin
# 或
go build ./...
```

### 2. 配置 `config.json`

```jsonc
{
  "listen": ":7865",
  "api_key": "",            // 留空表示不鉴权；建议设置为随机长串
  "auth_dir": "./auths",
  "region": "cn",           // cn=大陆 | global=海外
  "mode": "auto"            // auto | openai | relay
}
```

### 3. 登录账号

```bash
./login.sh                      # 交互输入手机号 + 验证码
./login.sh <手机号> <验证码>   # 非交互
AUTH_DIR=/path/to/auths ./login.sh   # 指定目录
```

成功后会在 `auths/` 生成 `autoclaw-<userId>.json`，包含 access/refresh token、沙箱 id/endpoint 缓存。

### 4. 运行服务

```bash
./bin/autoclaw-server -config config.json
# 或 Docker
docker build -t autoclaw2api .
docker run -d -p 7865:7865 \
  -v $PWD/auths:/app/auths -v $PWD/data:/app/data \
  -e AC2A_API_KEY=一个随机密钥 \
  autoclaw2api
```

### 5. 调用

```bash
curl http://127.0.0.1:7865/v1/chat/completions \
  -H "Authorization: Bearer $AC2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"openclaw","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

作为 OpenAI 兼容服务可直接配置进 `chatgpt-oa` / `claude-code` / `Cline` / `CherryStudio` 等客户端（Base URL 填 `http://127.0.0.1:7865/v1`）。

### 6. 查看状态

```bash
curl http://127.0.0.1:7865/status
./credit.sh                      # 积分日报
./credit.sh -json                # 机器可读
./maintain.sh                    # 手动跑一轮维护
```

## 账号池与错误分类

| 上游表现 | 处理 |
|---|---|
| 账户余额/额度不足（`code=402` 或正文命中「余额不足/insufficient」）| 冷却账号 `12h`（`cooldown.hard_credit`），期间不再选中 |
| HTTP `429` | 冷却 `60s`（`rt`）|
| 连续报错次数达阈值 | 冷却 `10m` |
| token 失效（`code=410001`/`401`/`Unauthorized`） | 判定会话死亡 → **禁用**，需人工重登后恢复（`ReenableAfterManual`/重启自动重扫凭证目录即 `enable`）|
| 所有账号不可用 | 返回 OpenAI 风格的 `503 no_healthy_account` |

积分刷新：维护日程每 30 分钟拉一次余额，余额恢复后自动解冻被冷却的账号。

> 关于「每日签到」：AutoClaw **没有**“签到点一下领积分”这类端点——`webElectronApi` bundle 里只有钱包查询（`wallet-instances`，其中 `daily` 分类即每日赠送额度 Gift Credits，**系统自动发放、无需主动领取**）和一次性任务（新手 `identity-tasks`、激励中心 `inspiration-task`、推广 `promotion-reward`）。因此本项目不做每日签到任务；每日免费额度到账后，调度器定时拉积分即可自动恢复账号。

## 支持的环境变量覆盖（`AC2A_*`）

| 变量 | 覆盖配置项 |
|---|---|
| `AC2A_LISTEN` | `listen` |
| `AC2A_API_KEY` | `api_key` |
| `AC2A_AUTH_DIR` | `auth_dir` |
| `AC2A_STATE_FILE` | `state_file` |
| `AC2A_REGION` | `region` |
| `AC2A_MODE` | `mode` |
| `AC2A_HARD_CREDIT` | `cooldown.hard_credit` |
| `AC2A_SOFT_RATE` | `cooldown.soft_rate` |
| `AC2A_ERR_COOLDOWN` | `cooldown.err_cooldown` |
| `AC2A_MAINTENANCE_MINUTES` | `schedule.maintenance_minutes` |
| `AC2A_USER_API_BASE` | `upstream.user_api_base_override`（调试） |
| `AC2A_RELAY_BASE` | `upstream.upstream.relay_base_override`（调试） |

## 海外（global）账号说明

AutoClaw 国内区用手机号+短信。海外区支持 Google / Z.AI 的 OAuth 登录（`SendCode` 不适用），本项目不直接实现 OAuth 前端；可通过以下方式获得 `access_token` 后手写凭证文件放进 `autoclaw-*.json`（字段与 `auths/` 内文件一致，`region` 填 `global`）。参见 `docs/`。

## 模型说明

`/v1/models` 先尝试从沙箱网关拉取真实 agent 列表，失败回退到静态表：
- `openclaw`：默认 agent（OpenClaw gateway 的模型名）
- `openclaw/default` / `openclaw/<agentId>`：指定 agent

> **模型名兼容性**：relay 通道不校验模型名——传 `glm-5.3-flash` / `glm-5.2` / `zai_auto` 等任意名称也会**回落默认 agent** 正常回复（不再报错）。后端模型的精确选择只在沙箱原生 OpenAI 端点启用时经 `x-openclaw-model` 头支持；模型列表未列出它们是为了防止客户端误选。

## 进阶说明

对话转发两种模式区别：

1. **`openai`** 直连沙箱 `POST /v1/chat/completions`（前提：沙箱 OpenClaw gateway 开了 `chatCompletions`，一般在本地配置里 `openaiCompletionsPath`）。
2. **`relay`** 用 `POST {relay}/api/electron/agent/send` + `GET {relay}/api/events`（SSE）自行拼 OpenAI 流式块，事件为实测 `agent:stream`（累积 `delta` 快照 + `type: text|done|error`）。这是 AutoClaw 官方 Web 前端实际走的路子，能拿 `thinking`、`tool_use`、`usage`。默认 `auto` 会先试 openai 端点、失败回退 relay。

> **重要**：AutoClaw 云龙虾设备必须通过 WebSocket 才能真正上线。relay 前需要先 `wss://{relay}/v1/client/ws?device_id=<sandbox_id>&access_token=<token>` 建连 + `auth.inject`，否则 `agent/send` 返回「云端设备未连接，请先唤醒云龙虾」。本项目内置纯标准库 WS 客户端（`internal/upstream/wsclient.go`）在此建连保活，设备上线后才走 HTTP bridge 收发。

## 测试

```bash
go test ./...        # 覆盖签名、错误分类、池轮换、SSE 聚合、relay 事件转换等
```

## 免责声明

本项目与 AutoClaw / Zhipu / Z.AI 无隶属关系。接口与截至编写时点的 Web 前端逆向约定，可能随上游更新失效；使用风险自负。
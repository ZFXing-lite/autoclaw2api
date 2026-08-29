# 逆向备忘

本项目从 https://autoclaw.z.ai/web/ 的前端 bundle 逆向出 AutoClaw 云端沙箱的接口约定。以下是关键结论，便于后续维护与排查。

## 基础

- Web SPA 实际加载：`https://autoclaw.z.ai/web/`（其 CNAME 指向国内 CDN）。
- 用户 API（userapi）REST 基址分地区：
  - 大陆：`https://autoglm-acceleration-api.zhipuai.cn`
  - 海外：`https://autoglm-api.autoglm.ai`
- 统一业务信封：`{"code","msg","time","trace","data"}`。`code=0` 成功。
- 关键错误码：
  - `410000`（0x41e4）＝未登录/会话失效。
  - `630202`＝验证码错误/失效。
- 沙箱凭据接口（`/agentdr`）挂在同一域名，路径 `/agentdr/v2/assistant/sandbox/*`。

## 2. 签名

浏览器端请求头（`webElectronApi` chunk 常量反编译）：

```
X-Version:       1.12.1
X-Tm:            web
X-Product:       autoclaw
X-Client-Type:   web
X-Channel:       official
X-Auth-Appid:    100003
X-Auth-TimeStamp: <unix 秒>
X-Auth-Sign:     MD5("100003&<unix秒>&38d2391985e2369a5fb8227d8e6cd5e5")
X-Trace-Id:      <uuid>
X-Lang:          en
Authorization:   Bearer <accessToken>   (需登录的接口)
```

签名在 `internal/upstream/headers.go` 实现，`Sign(ts)`。

## 3. 登录流程

见 `cmd/login`。短信验证码 + 手机号：

- `POST /userapi/v1/agent-send-code` `{source_id:"autoclaw", device_id, phone}`
- `POST /userapi/v1/agent-login/`（注意尾部 `/`，服务端 307 到无斜杠）`{source_id, device_id, phone, code, platform:"web"}`

返回 `access_token/refresh_token/user_id/user_name/first_login/...`。响应里没有 `expires_in`，本项目保守缓存 28 天并定时提前刷新。

## 4. 沙箱与网关

- 每个账号有自己独立的 OpenClaw 云端沙箱（`sandbox_id` / `sandbox_endpoint`）。
- `/agentdr/v2/assistant/sandbox/list` 返回 `sandbox_list[]`（含 `sandbox_status`、`end_timestamp`）。
- 可用时缓存到凭证文件的 `sandboxId/sandboxEndpoint/endTimestamp`，避免每次都拉。
- 沙箱网关（relay）按 归一化基址 `{endpoint}/autoclaw-cloud` 访问，`ws://`→`http://`、`wss://`→`https://`（对齐官方 `L()`：去掉 query/hash、截断到 `/autoclaw-cloud`）。
- **两个 base 分工**（实测确认，勿混淆）：
  - 裸 base `L(endpoint)`：`/v1/sandboxes/{sandbox_id}/wake|status`。
  - proxy base `L(endpoint)/proxy/{sandbox_id}`：`/api/electron/*`、`/api/events`、`/health`、OpenAI 端点。
- 网关两根路径：
  - 唤醒/状态：`{裸base}/v1/sandboxes/{sandbox_id}/wake`（body `{access_hold_ms, reason, apply_business_busy_cooldown}`）、`/status`。
  - OpenAI 端点：`POST {proxy}/v1/chat/completions`、`GET {proxy}/v1/models`（需网关配置 `chatCompletions` 打开，默认多半是关的）。
  - relay bridge：`POST {proxy}/api/electron/agent/send`（`{"args":[{sessionKey, message, thinking?}]}`，注意 **`message` 必须是字符串**，对象形式返回 `message is required for agent.send`）+ `GET {proxy}/api/events`（SSE）。
  - **model 字段陷阱（已实测）**：`agent/send` 的 `model` 字段只认 agent 目标名；透传后端模型名（如 `glm-5.3-flash`）上游直接报 `{"type":"error","delta":"LLM request failed."}`。本项目 relay 通道因此**不透传 model**，任何客户端模型名统一回落默认 agent；后端模型选择仅由 OpenAI 端点 `x-openclaw-model` 头支持。
  - `Authorization: Bearer <accessToken>`（幂等：登录返回的 access_token 值本身自带 `Bearer ` 前缀）。

## 4.1 设备上线必须走 WebSocket（关键）

仅 HTTP bridge（`agent/send`）会得到「云端设备未连接，请先唤醒云龙虾」。真实可用流程（本项目实现）：
1. `wss://{L(endpoint)}/v1/client/ws?device_id=<sandbox_id>&access_token=<token无Bearer>`（`ro` 把 https→wss）
2. 发 `{"type":"auth.inject","token":<无Bearer>,"userId":..,"clientMetadata":{...}}`，等 `{"type":"auth.inject.ok"}`
3. 设备上线后，对话才走 HTTP bridge：`POST agent/send` → 返回 `{ok:true, runId}`；事件从 `/api/events`（SSE）回流。

纯 stdlib WS 客户端在 `internal/upstream/wsclient.go`（`wsclient`），接入在 `openDeviceWS`。

## 5. 事件流（relay）格式

`/api/events` 每帧 `data: {"event": "...", "payload": {...}}`。

- **实测主格式 `event="agent:stream"`**：payload `{runId, type: "phase"|"text"|"tool_call"|"tool_result"|"done"|"error", phase?, delta, sessionKey}`。其中 `type="text"` 的 `delta` 是**累积快照**（前缀递增，`emitText` 做前缀差分发增量）；`type="done"` 表示会话结束（写 usage + `[DONE]`），`sessionKey` 形如 `agent:main:main`（用 `runId` 归属过滤即可）。
- 兼容 `event="chat"`：payload `{runId, sessionKey|to, state, message:{content(...), toolCalls}, usage}`。
- `event="agent"`：payload `{runId, stream: "assistant"|"tool"|"compaction"|"error"|"lifecycle", data}`。
- 忽略 `gateway:status` / `gateway:event`（health/tick/presence）等噪音帧。

转换逻辑在 `internal/upstream/relay.go`：按 `runId` 归属过滤；输出 OpenAI 流式块（`content`、`reasoning_content`、`tool_calls`、`finish_reason`、`usage`、`[DONE]`）。

## 6. 积分

- `GET /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw` → `total_balance`（可退 `v1` 钱包接口兜底）。
- AutoClaw 无每日签到：新用户/活动是一次性奖励，所以调度器只做积分刷新（解冻冷却账号），不做倒计时钟。

## 7. 海外 OAuth（未内置）

海外区走 OAuth，端点形如：
- `POST /userapi/overseasv1/zai-oauth-url` / `-google-oauth-url`（拿登录 URL）
- `POST /userapi/overseasv1/zai-oauth-login` / `google-oauth-login`

本仓库不实现 OAuth 前端；`credits.sh`/`auths/*.json` 支持你手工填充海外 token（把 `region` 写成 `global`）。如需，可用真实设备的 Web 登录拿 token 后转成仓库格式。
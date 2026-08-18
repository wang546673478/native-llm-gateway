# 方案：Claude Code 环境指纹归一化（无副作用版）

> 状态：定稿待审 · 2026-08-18
> 目标：给「多台机器（亲友）经网关共用一个上游 key」场景，抹平设备级多头指纹，降低被封号风险，但**绝不引入任何功能副作用**。

---

## 一、背景与范围

### 要解决的问题

Claude Code 客户端往上游 Anthropic 发的请求里，夹了几处「能识别出『这把 key 被多台机器/多个用户共享』」的结构化指纹（已从 access log 真实 body 核实）：

| 指纹 | 位置 | 多头暴露 | 有无功能副作用 |
|---|---|---|---|
| `device_id` | `metadata.user_id`（JSON 字符串子字段） | 每机器唯一，最强信号 | **无** |
| `Platform` / `Shell` / `OS Version` | `system[].text` 的 `# Environment` 块 | 每机器不同 | **无** |
| `Primary working directory` | 同上 | 每用户不同 | **有**（模型定位相对路径/读真实文件依赖它） |
| `x-anthropic-billing-header`（cc_version） | `system[0]` | 弱 | **有**（版本号与协议行为相关） |

### 明确不做（守住「无副作用」边界）

- ❌ 不碰 `Primary working directory` —— 它是功能字段，任何替换都会让「模型以为的工作目录」和「真实文件路径」脱节，导致相对路径解析、读文件、工具调用出错。此前讨论的「整段固定」「换 home 前缀」「记录+替换」都无法绕过「工具在用户本机执行时用的是真实路径」这个硬边界。
- ❌ 不碰 `messages` 对话内容、`tools`、`thinking`、`max_tokens` 等。
- ❌ 不碰 `billing header`（默认保留真实值）。

### 只做的（纯指纹、无副作用）

| 字段 | 替换成 |
|---|---|
| `device_id` | config 配了 `canonical_device_id` 就用它；没配则启动时随机生成一个固定值存内存 |
| `Platform` | 网关 `runtime.GOOS` 真值 |
| `Shell` | 网关 `$SHELL` 真值 |
| `OS Version` | 启动时 `exec uname -r` 真值 |

---

## 二、落点设计（遵循 CLAUDE.md 低耦合高内聚）

### 核心判断：这不是「协议面」职责，是「发往上游前的一次 body 变换」

三个协议 base（`openai_compatible` / `anthropic_compatible` / `google`）职责是「各自协议透传」。把指纹归一塞进去会重复三份、且与协议无关。所以放**公共的、三个 base 都必经的上游出口之前**。

### 最终落点：`proxy` 层构造 `provider.Request` 处

在 `proxy.go` 第 292 行构造 `req := &provider.Request{... Body: body ...}` 时，对 `body` 做一次归一，三个协议面自动全生效（因为它们拿到的 `req.Body` 已经归一过），**一处改、全生效**。

```
proxy.HandleRequest
  └─ proxy.handle
       └─ req := &provider.Request{ Body: sanitize(body) }   ← 在这里触发一次
             └─ provider.SendRequest(req)   → 三个 base 拿到归一后的 body
```

---

## 三、组件设计

### 1. 新包 `internal/fingerprint`（单一职责）

```
backend/internal/fingerprint/
├── fingerprint.go      # 核心：Sanitize(body, Snapshot) + Snapshot 采集
└── fingerprint_test.go # 单测（见测试点）
```

**职责**：把「Claude Code/Anthropic 请求 body 里的设备级指纹」归一成一套固定值。只做「定位 + 替换」这一件事。不 import `proxy`/`provider` 的可变业务，避免循环依赖。

### 2. `Snapshot`（启动采集一次，存内存）

```go
// Snapshot 网关自己的环境快照 —— 启动时采集一次，内存只读，替换时直接引用
type Snapshot struct {
    DeviceID  string // config canonical_device_id；空则随机生成一次
    Platform  string // runtime.GOOS
    Shell     string // $SHELL，空则 "bash"
    OSVersion string // exec "uname -r"（失败则 fallback runtime.GOOS）
}

// Capture 启动时采集一次真实环境 + device_id
func Capture(canonicalDeviceID string) Snapshot
```

- `uname -r` 失败（如某些容器/Windows）→ fallback 到 `runtime.GOOS`，不 panic。
- 只在启动时调一次，热重载不重复采集，替换时零开销。

### 3. `Sanitize`（定位 + 替换，某协议 body 原样返回即 no-op）

```go
// Sanitize 把 body 里的设备指纹归一成 snap 的固定值。
// body 非法 JSON / 无 fingerprint 字段 / 开关关闭时，原样返回（透传语义不变）。
// 只改 metadata.user_id.device_id 与 system[].text 里 # Environment 块三个字段，
// 不碰 messages / tools / thinking / workdir。
func Sanitize(body []byte, snap Snapshot) []byte
```

实现要点：
1. `json.Unmarshal` 到 `map[string]any`；Unmarshal 失败 → 原样返回。
2. 若 `metadata.user_id` 是 JSON 字符串 → 解出 `device_id`，替换为 `snap.DeviceID`，重新 marshal 回字符串回填。
3. `system` 若为数组 → 遍历每个块的 `text`，定位 `# Environment` 段，把 `Platform: x` / `Shell: x` / `OS Version: x` 三行替换为 `snap` 对应值。
4. 转换后重新 `json.Marshal` 返回新 body。

### 4. 开关（config，默认开）

`config.Config` 新增一节：

```go
type FingerprintConfig struct {
    // Enabled 是否归一化设备指纹。默认 true。
    Enabled bool `mapstructure:"enabled"`
    // CanonicalDeviceID 统一 device_id；空则启动时随机生成一次(存内存，不落盘)。
    CanonicalDeviceID string `mapstructure:"canonical_device_id"`
}
Fingerprint FingerprintConfig `mapstructure:"fingerprint"`
```

- `Enabled` 默认 true：`Load` 后若值缺失需要显式置 true（viper 的 bool 零值是 false，需在 load 补默认）。这一点要在实现时核对 config 的默认值注入机制，不能直接零值当 false 导致「默认关」。
- 关 = `Sanitize` 不调用，body 零改动零开销，保持现有透传行为。

### 5. 注入（复刻 `QuotaChecker` 范式）

`proxy.Config` 增加一个可选字段，`Engine` 持有一个 hook：

```go
// proxy.Config
FingerprintSanitizer func(body []byte) []byte  // nil = 不归一

// proxy.handle 构造 req 前
if e.fingerprintSanitizer != nil {
    body = e.fingerprintSanitizer(body)
}
```

- `server` 组装 Engine 时，根据 `cfg.Fingerprint.Enabled` 决定注入这个函数（闭包捕获启动时 `fingerprint.Capture` 得到的 Snapshot），还是置 nil。
- **proxy 不直接 import fingerprint 包**——由 server 注入闭包，符合「接口注入/回调」而非跨包直接依赖（与 `QuotaChecker` 完全同构）。

### 6. 网页端开关 + 热更新

- 仿照已有 `PUT /routing/order` 的先例，加个管理端点改 `fingerprint.enabled` / `canonical_device_id`，或走 config 写回 → `Watcher` 触发 `srv.Reload`。
- 热更新只切换 `Enabled`（重新注入 sanitizer）；`CanonicalDeviceID` 改了要重启生效（因为它影响启动时 Capture 的 Snapshot），或热更新时重新 Capture。**实现时定为「Enabled 即时、CanonicalDeviceID 需重启」并加 Warn 提示**（与项目现有「hot-reload 需重启字段」风格一致）。

---

## 四、需同步修改的文件清单

| 文件 | 改动 |
|---|---|
| `backend/internal/config/config.go` | 加 `FingerprintConfig` + `Config.Fingerprint` 字段 |
| `backend/internal/fingerprint/fingerprint.go` | 新，`Snapshot` + `Capture` + `Sanitize` |
| `backend/internal/fingerprint/fingerprint_test.go` | 新，单测 |
| `backend/internal/proxy/proxy.go` | `Config` 加 `FingerprintSanitizer`；`handle` 构造 req 前调它 |
| `backend/internal/server/server.go` | 组装 Engine 时注入（根据 config 开关） |
| config 三模板（config.yaml / example / docker） | 加 `fingerprint` 块 |

---

## 五、测试点（写单测时覆盖）

1. **device_id 替换**：body 里 `metadata.user_id` 含真实 device_id → `Sanitize` 后 = snap.DeviceID；`account_uuid`/`session_id` 不动。
2. **环境块替换**：`# Environment` 里 Platform/Shell/OS Version → snap 对应值；`Primary working directory` **保持原样**（无副作用核心断言）。
3. **对话内容零污染**：`messages` 里若恰好出现 `linux`/`bash` 等词，不被误改（环境块替换只发生在 `# Environment` 段内）。
4. **非法 JSON / 非 anthropic 结构**：原样返回，不 panic、不误改。
5. **device_id 为空**：`metadata.user_id` 无 device_id 字段时不新增、不崩。
6. **Snapshot fallback**：`uname -r` 失败时 OSVersion 落到 `runtime.GOOS`。
7. **开关关闭**：`FingerprintSanitizer == nil` 时 handle 路径零改动（透传不变）。

---

## 六、验证命令（改完必跑）

```bash
make test        # 全量
make vet         # go vet
make build       # 编译
cd frontend && npx tsc --noEmit   # 前端（若动到网页端开关才需要）
```

---

## 七、未决/边界（如实标注）

- **合规边界**：这是「绕开 Anthropic 多设备共享检测」的灰色操作。CC Gateway 同行的 ToS 明确「intended for managing your own devices under one subscription, not account sharing」。给亲友共用已跨线，此方案只做技术实现，合规风险由用户自行判断。
- **device_id 归一后的反噬**：所有亲友同一 device_id，若将来 Anthropic 用「device_id 精确一致 + 其他维度差异」作新特征，或需要区分用户做用量统计，「统一」会不便。故 `CanonicalDeviceID` 做成可配，留出「换固定值 / 关闭」的口子。
- **不保证绝对不封**：并发量 / QPS 异常（多用户同时刷）不在本方案覆盖，那是「量」的问题，非「指纹」问题。

# ChinaCPA 🇨🇳 国产大模型反代网关

> ⚠️ **本项目仅供个人学习研究使用**，请勿用于任何商业用途或违反各平台服务条款的场景。
>
> 本仓库为**二创（衍生）版本**，在上游基础上继续开发，主要参考两个项目：
> [XingQiPan/ChinaCPA](https://github.com/XingQiPan/ChinaCPA)（网关 / 面板 / 平台适配的全部基础）
> 与 [xu20060420fan-cell/workbuddy2api-trae-qoder-minimax-panel](https://github.com/xu20060420fan-cell/workbuddy2api-trae-qoder-minimax-panel)（WorkBuddy 成长任务事件链、Trae / Qoder / MiniMax 面板口径）。
> 本仓库相对上游新增了什么，见文末「[🛠 相对上游的改动](#-相对上游的改动)」；出处与致谢见「参考」。
>
> 一站式纳管多家国产 AI 平台账号，自动签到攒积分、自动刷新额度、自动统计计费，
> 一个 OpenAI 兼容接口随便调 —— **无需下载任何官方客户端**。
>
> 「每日签到」分两类：**接口型**（有真实签到/领取端点：WorkBuddy、Trae、Qoder）和
> **确认型**（上游无签到接口，发放由登录/服务端触发，本端查钱包/积分余额确认到账，
> 未到账不算失败、当日自动复查：千问办公；Qwen、豆包无每日积分体系）。

| 平台 | 登录方式 | 免费额度 | 每日签到 |
|---|---|---|---|
| **WorkBuddy / CodeBuddy**（腾讯，CN codebuddy.cn / 国际 workbuddy.ai） | OAuth 浏览器授权（双区独立登录） | 积分制（资源包） | ✅ 每日签到 + 任务中心「自动完成 + 领奖」（见下）|
| **Qwen 通义千问**（chat.qwen.ai） | 粘贴网页 token（OAuth 免费层 2026-04 已下线） | 网页会话免费额度 | —（无签到体系，本地自动计数） |
| **Trae**（字节 SOLO 免费通道，CN/国际） | PKCE 授权码回调 | ide_credits | ✅ 每日 +200 积分 |
| **Qoder**（阿里，CN qoder.com.cn / 国际 qoder.sh） | 控制台 PAT 导入 | credits（套餐+加购+组织） | ✅ 每日积分活动自动领取 |
| **千问办公**（阿里 QwenWork，gateway.qwenwork.cn） | 钉钉扫码授权（device-flow） | 积分制（个人/企业池） | ☑ 确认型：每日积分自动确认（登录触发发放，个人版 100~200/天当日有效） |
| **豆包**（字节，www.doubao.com） | 豆包 App 扫码自动回调 / 窗口登录 / Cookie 兜底 | 网页通道（文 / 图 / 视频按豆包 App 权益） | ☑ 浏览器通道：开启 `doubao.checkin_via_browser` 后无头 Chrome 读活动接口并领取 |
| **MiniMax**（MiniMax Code，agent.minimax.cn / 国际 .io） | OAuth 设备码授权（RFC 8628 + PKCE）/ 读本机 `~/.minimax/auth` / 粘贴 token | 积分制（400/天签到 + 资源包） | ✅ 每日签到 +400 积分（领取后回查状态确认） |

> Trae 2026-10 起切换到 PKCE 授权码登录（客户端 3.3.62），本项目已跟进新流程；旧版 refreshToken 凭证仍可粘贴导入。
> WorkBuddy 分区：国内版（codebuddy.cn）与国际版（workbuddy.ai）面板各一张卡片，账号、额度、签到、模型完全独立；模型前缀 `workbuddy:` / `workbuddy-intl:`。
> Qoder 分区：国内版 PAT 直接粘贴；国际版加前缀 `global:`。
> 千问办公：面板发起后用钉钉扫码完成授权；挡位模型 `flash` / `pro` / `qwen3.8-max-preview`（云端目录自动拉取），也可粘贴 `ory_rt_` 开头的 refresh token 导入。
> MiniMax：面板「添加账号 → MiniMax」用**设备码授权**接入（浏览器打开链接、确认设备码即完成），到期自动续期。也可以粘贴凭证留空直接读本机 MiniMax Code 登录态，但那份 `refresh_token` 与客户端共用、网关不代续期（续期会顶掉客户端登录），只适合客户端不用的号。

## ✨ 功能

- **凭证即用**：面板 OAuth 授权 / 扫码 / PAT / refreshToken / Cookie 多种纳管方式，不需要安装官方 IDE/CLI
- **七大平台**：WorkBuddy（CN/国际）、Qwen、Trae（CN/国际）、Qoder（CN/国际）、千问办公、豆包、MiniMax（CN/国际），模型前缀 `workbuddy:` `workbuddy-intl:` `qwen:` `trae:` `qoder:` `qwenwork:` `doubao:` `minimax:` `minimax-intl:`
- **豆包文 / 图 / 视频**：App 扫码自动回调纳管（字节 passport 协议）；`doubao-chat` 网页助手对话（OpenAI 兼容流式）、Seedream 图片生成（OpenAI Images 协议）、Seedance 视频生成（火山方舟 Ark 异步任务协议，自动解析无水印原片）
- **自动签到积分**：每日 9 点、21 点自动签到（可配），**时点抖动 + 全局并发闸门 + 指数退避重试 + 当日截止**（见下方「签到排程」）；WorkBuddy 任务中心自动接受任务+领取奖励。无签到接口的平台（千问办公确认型 / Qwen 无体系 / 豆包需开浏览器通道）在面板以「说明」标签呈现，不计为失败
- **签到排程可调**：`checkin_concurrency` 并发上限、`checkin_jitter_minutes` 时点抖动、`checkin_retry_minutes` 退避序列、`checkin_deadline` 当日截止 —— 多账号自用最怕的就是「同一秒 N 个号齐发」
- **签到记录带时刻**：`state.json` 的签到记录含 `phase`（claimed 真领到 / already 上游已签 / info 无体系 / failed）与 `at`（观察时刻），保留 30 天；面板显示「✓ 领取 09:03」而不只是「成功」
- **签到结果推送**：通用 Webhook / Telegram / Server 酱，一次签到结算后推送**一条汇总**（成功/已签/无体系/失败 + 失败明细），不做 N 条轰炸。恰好是「一个人用」最缺的那一环
- **豆包签到（浏览器通道，可选）**：豆包签到接口带设备签名，必须借页面上下文调用 —— 开启 `doubao.checkin_via_browser` 后临时起一个无头 Chrome（约 20~40 秒）读活动接口状态并领取，顺手领已完成的任务奖励
- **豆包链路诊断**：面板「豆包生成额度 → 网络诊断」把导入凭证那条请求分「直连 / 环境变量代理 / Windows 系统代理」各投一遍，逐跳列出 DNS → TCP → TLS → HTTP 结果与证书签发者。用来区分「本机出网被拦 / 代理规则把国内站点甩境外 / 系统代理开关残留 / 上游改版」这四种都被用户说成「连不上」的情况；走代理失败时程序会自动改走直连重试一次
- **MiniMax 签到**：纯 token 级接口，不依赖浏览器。流程是「查状态 → 领取 → 回查确认」——领取返回成功但今日状态没变成已领时判失败让调度重试，避免把「接口接受了请求」当成「积分真到账」
- **额度自动获取**：每 10 分钟自动刷新各账号剩余额度（WorkBuddy 积分 / Trae ide_credits / Qwen 今日剩余次数 / MiniMax 积分）；豆包文/图/视频按账号本地日计数，重启不丢
- **模型发布 · 渠道归组**：面板自定义对外模型 → 多个上游渠道（跨平台），渠道间失败转移 + 可配渠道策略（优先级 / 轮询均衡），渠道内多账号轮询 + 会话黏性；`/v1/models` 只暴露已发布模型（未配置时默认暴露全部内部模型），配置落盘 `data/models.json`
- **OpenAI 兼容网关**：`/v1/models` + `/v1/chat/completions`（SSE 流式），多账号自动轮询 + 失败转移 + 限流冷却；上游凭证失效自动标记「需重新登录」并停止调度，重新导入即恢复
- **仪表板统计**：今日/7 日 token 消耗（100M/1B 风格）、请求数、按模型费用估算（发布模型单价优先，对话 $/1M、图片/视频 $/次 → ¥ 自动折算）、剩余额度自动计算
- **仪表盘卡片化**：每张卡改成「固定卡头 + 卡体独立滚动」，标题常驻、内容自己在卡内滚；有未完内容时卡底出现羽化渐隐，滚到底自动消失；卡头右上角「展开 / 收起」可把单卡就地摊开；窗口 ≤900px 时自动回落为整页滚动，避免嵌套滚动的手感
- **单实例守卫**：启动先抢监听端口，第二个实例 1 秒内以中文提示「端口已被占用（PID xxx）」退出，不会再出现两个进程抢同一个 bbolt 锁、刷 `open request db: timeout` 的情况；启动横幅打印 exe 构建时间，方便确认跑的是不是新版本
- **一键打包分发**：`./build_dist.sh` 一次产出 Windows zip + Linux amd64/arm64 tar.gz（纯静态单文件、不带 libc 依赖，内含启动脚本、systemd 单元与 SHA256 校验），目标机不需要装 Go
- **请求日志与推送**：bbolt 请求审计（范围聚合）、签到结果汇总推送（Webhook / Telegram / Server 酱，一次结算一条，不做 N 条轰炸）
- **Qwen 每日限流**：`daily_request_limit` 达上限自动返回额度错误并切换到下一账号/渠道
- **单二进制**：纯 Go 标准库，零第三方依赖，下载即用

## 🚀 快速开始

```bash
# 1. 构建（或直接下载 release 的 chinacpa.exe）
go build -o chinacpa.exe ./cmd/chinacpa

# 2. 启动（首次自动生成 config.json）
./chinacpa.exe -config config.json
# Windows 双击 start.bat 也可以

# 3. 打开面板
# http://127.0.0.1:8313/   默认密码 123456
```

### 二进制包（不想装 Go）

```bash
./build_dist.sh          # 一次产出全部平台，产物在 dist/
```

| 产物 | 平台 | 用法 |
| --- | --- | --- |
| `ChinaCPA-windows-amd64.zip` | Windows x64 | 解压 → 双击 `start.bat` |
| `ChinaCPA-linux-amd64.tar.gz` | Linux x86_64 | `tar xzf … && cd ChinaCPA-linux-amd64 && ./start.sh` |
| `ChinaCPA-linux-arm64.tar.gz` | Linux ARM64（ARM 服务器 / 群晖 / 树莓派） | 同上 |

- 静态单文件，目标机不需要装 Go，也不依赖 libc（glibc / musl 都能跑）。
- 发行包内 `config.json` 默认 `"listen": "127.0.0.1:8313"`（只监听本机）。要给同网段设备访问，改成 `":8313"`，并务必先改掉 `panel_password`。
- Linux 后台常驻两种方式：`./start.sh -d`（日志写 `chinacpa.log`）；或改好 `chinacpa.service` 里的路径后 `cp chinacpa.service /etc/systemd/system/ && systemctl daemon-reload && systemctl enable --now chinacpa`。
- 豆包「浏览器签到 / 窗口扫码登录」需要目标机装了 Chrome / Chromium（`/usr/bin/google-chrome`、`/usr/bin/chromium` 会自动探测，也可在 `doubao.chrome_path` 指定）；纯接口能力不需要浏览器。
- **单实例**：同一 `data` 目录只允许一个进程。启动时先抢监听端口，重复启动会直接中文提示「端口已被占用（PID xxx）」并退出，
  不会出现第二个进程和第一个抢 bbolt 锁、刷 `open request db: timeout` 的情况。要并行跑就用 `-config` 指向另一份配置（含独立 `listen` 与 `data_dir`）。

### 面板操作

1. 点右上角「添加账号」→ 选平台 → 打开授权链接登录 → 自动纳管 + 自动签到 + 自动拉额度
2. 仪表板实时看：账号池状态、剩余额度、今日用量、费用估算
3. 网关信息区有现成的接入参数（地址 + 密钥）

### 客户端接入（三种原生协议）

网关同时支持三种请求协议（翻译层完整移植自 CLIProxyAPI）：

| 端点 | 协议 | 适用客户端 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | OpenAI SDK / 主流龙虾 |
| `POST /v1/responses` | OpenAI Responses API | **Codex CLI**（wire_api=responses） |
| `POST /v1/messages` | Anthropic Messages | **Claude Code**（x-api-key 鉴权） |

**模型别名**：Codex / Claude Code 客户端可能写死模型名，在 config.json 配置映射：

```json
{
  "model_aliases": {
    "gpt-5.2-codex": "workbuddy:glm-5.3",
    "claude-sonnet-4-6": "trae:glm-5.2"
  }
}
```

**模型发布（推荐）**：面板「模型发布」区可视化配置对外模型，一个名字挂多个上游渠道，
渠道自动轮询 + 失败转移，并附带单价（对话模型 $/1M 输入/输出；图片/视频模型 $/次）用于费用估算。
发布模型可选**类型**：`chat`（默认，走 `/v1/chat/completions`）、`image`（走 `/v1/images/generations`）、
`video`（走 `/api/v3/contents/generations/tasks`）；类型不匹配的调用会被网关拦下并提示正确端点。
配置保存在 `data/models.json`，也可手写：

```json
{
  "usd_cny": 7.2,
  "models": [
    {
      "id": "gpt-5",
      "note": "主力",
      "pricing": { "input": 0.002, "output": 0.008 },
      "channels": [
        { "provider": "qwen", "model": "qwen3-coder-plus" },
        { "provider": "workbuddy", "model": "glm-5.3" }
      ]
    },
    {
      "id": "my-seedream",
      "type": "image",
      "pricing": { "input": 0.02 },
      "channels": [ { "provider": "doubao", "model": "seedream-5.0-flash" } ]
    },
    {
      "id": "my-seedance",
      "type": "video",
      "channels": [ { "provider": "doubao", "model": "seedance-2.5" } ]
    }
  ]
}
```

客户端直接 `{"model":"gpt-5"}` 调用即可；配置了发布模型后 `/v1/models` 只返回已发布模型。
各平台原始模型仍可 `平台:模型` 直连（面板「各平台模型 · 计价倍率」区可查全部原始模型与 credits 系数）。

**Codex CLI 接入**（`~/.codex/config.toml`）：

```toml
model = "gpt-5.2-codex"            # 经 model_aliases 映射，或直接写 workbuddy:glm-5.3
model_provider = "chinacpa"
wire_api = "responses"

[model_providers.chinacpa]
name = "ChinaCPA"
base_url = "http://127.0.0.1:8313/v1"
env_key = "CHINACPA_API_KEY"       # export CHINACPA_API_KEY=<你的 api_key>
```

**Claude Code 接入**：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8313
export ANTHROPIC_API_KEY=<你的 api_key>
export ANTHROPIC_MODEL=trae:glm-5.2        # 或用 model_aliases 映射 claude-sonnet-4-6
```

**标准 OpenAI SDK**：

```bash
curl http://127.0.0.1:8313/v1/chat/completions \
  -H "Authorization: Bearer <你的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen:qwen3-coder-plus",
    "stream": true,
    "messages": [{"role":"user","content":"你好"}]
  }'
```

模型命名：**`平台:模型`**（`workbuddy:deepseek-v4.1` / `qwen:qwen3-coder-plus` / `trae:glm-5.2`）。
裸模型名（如 `glm-5.2`）在唯一平台存在时可直接用；重名时接口会提示可用前缀。

### WorkBuddy 成长任务：自动完成 + 领奖

任务中心的积分不是「点一下就有」的：每条任务有自己的 `current/target`，进度由**客户端行为事件**
推进，光调 accept/claim 只能领到已经达标的奖励，推不动进度。

本项目的做法是把客户端的事件形状搬过来，用同一条上报通道补进度：

| 通道 | 端点 | 用于 |
|---|---|---|
| 桌面端指纹 | `POST copilot.tencent.com/v2/report` | 桌面端对话链（RichMeow_Chat）、进入 Buddy 应用、模板使用、灵感做同款、设计画布、定时任务 |
| CLI 端指纹 | `POST codebuddy.cn/v2/report`（`chat_request_send`） | 「和 AI 聊天 5 次」补量、Buddy 领养前置活跃 |
| Web 端指纹 | `POST workbuddy.cn/v2/report` | 页面行为类（体验资料库） |

**入口**：面板账号行「成长任务」→ 弹出任务清单（标题 / 进度条 / 可领状态 / 是否支持自动补进度）
→「一键完成」。

流程：列表 → 接受（默认口径与小程序口径**分开提交**，混提交会 task not found）→
对有动作且未达标的任务跑事件链 → 等上游异步结算（有界轮询）→ 领奖。

覆盖范围分两类：

**① 纯事件上报（零 token）**：`chat_5`、`first_buddy`、`RichMeow_Chat`、`Buddy_App`、
`Buddy_App_QQ`、`automation_1`、`Library_read`、`template_5`、`playbook_prompt`、
`create_canvas`、`Sequential_Tasks_1`、`Sequential_Tasks_2`。

**② 必须真实对话，但压到最小**：`expert_5`、`Expert_team_use_3`、`skill_1`、
`WorkBuddy_Valorant`、`expert_use_maidehao` / `_douyinyin` / `_shangjia_xuanchuan` / `_bowangwang`。

第二类的判据里 `requestId` 必须是服务端真实对话返回的 id（自造 UUID 不计数），所以躲不掉
一次真实请求，但可以做到几乎免费：

- 输入只发一个字符 `1`，不带 system prompt，`max_tokens=16`；
- **一次对话同时点亮多条任务**：专家的「召唤 + 对话 + 使用」链既给 `expert_5` 计一次，
  也点亮对应的 `expert_use_xxx`，5 条具名任务共 5 次对话，不是 5+5；
- 先跑具名任务，再回读真实进度决定 `expert_5` / 专家团还差几次，已达标不重复打；
- 专家 id 直接从任务元数据的 `jump_url`（`expertId%3Dex_xxx`）取，不在 4 万个专家里瞎搜。

实测（单账号，19 条任务）：两轮共领 **+700 积分**，额度消耗 **6**（used 177 → 183）。

**唯一做不了的一条**：`wb_wechat_oa_subscribe_task`（关注公众号）—— 需要真的在微信里关注，
没有任何接口可代做，只能手动。

判定口径：`target > 0 && current >= target` 且未领过即视为可领（**不看** `accept_status`：
上游回读时会把 `completed` 短暂写回，据此领奖会误领）。进度字段在 `progress` 子对象里，
不在平铺的 `current/target` 上。

### 豆包：语言模型 + 图片生成 + 视频生成

豆包通道基于网页协议（www.doubao.com）。**推荐扫码登录**：面板「添加账号 → 豆包」→
弹窗直接显示二维码（或打开链接）→ 豆包 App「对话 → ＋ → 扫一扫」→ 手机确认后
**自动回调纳管**（字节 passport 扫码协议，服务端取 Cookie，无需浏览器）。
兜底方式一（推荐，扫码接口出问题也能用）：「添加账号 → 豆包」→ **「打开浏览器窗口登录」**。
程序弹出一个独立浏览器窗口（临时配置目录，不动你日常 Chrome），你在里面照常登录豆包，
登录成功后自动取回登录态、关窗、纳管 —— 不复制任何东西，也不依赖豆包的登录接口。
兜底方式二：登录 www.doubao.com 后 F12 → Network → 任一请求 → 复制完整 `Cookie:`
头（需含 `sessionid` 与 `s_v_web_id`）粘贴导入。设备标识（device_id/web_id）自动生成落盘。

**可用模型**：

| 模型 | 类型 | 说明 |
|---|---|---|
| `doubao-chat` | 文本 | 网页默认助手（OpenAI 兼容，流式/非流式） |
| `seedream-5.0-flash` / `seedream-5.0-pro` / `seedream-4.5` / `seedream-4.0` | 图片 | Seedream 网页通道 |
| `seedance-2.5` / `seedance-2.0` / `seedance-2.0-pro` / `seedance-2.0-fast` / `seedance-2.0-mini` / `seedance-1.0` | 视频 | Seedance 网页通道（seedance-2.5 支持 30s） |

**图片生成**（OpenAI Images 协议，`size` 自动换算为 Seedream 比例，也可直接传 `ratio`）：

```bash
curl http://127.0.0.1:8318/v1/images/generations \
  -H "Authorization: Bearer <你的 api_key>" -H "Content-Type: application/json" \
  -d '{"model":"doubao:seedream-5.0-flash","prompt":"一只猫","size":"2048x2048","n":4}'
```

**视频生成**（火山方舟 Ark 异步任务协议，`/api/v3/contents/generations/tasks`；也支持 `POST /v1/videos` 别名。
提示词支持 Ark 风格参数 `--ratio 16:9 --duration 5 --resolution 720p`；任务在内存中跟踪，服务重启后进行中任务会丢失）：

```bash
# 创建任务
curl http://127.0.0.1:8318/api/v3/contents/generations/tasks \
  -H "Authorization: Bearer <你的 api_key>" -H "Content-Type: application/json" \
  -d '{"model":"doubao:seedance-2.5","content":[{"type":"text","text":"夕阳下奔跑的少年 --ratio 16:9 --duration 5"}]}'
# → {"id":"cgt-xxxx...","status":"running",...}

# 轮询（succeeded 后 content.video_url 即结果；优先解析素材库无水印原片）
curl http://127.0.0.1:8318/api/v3/contents/generations/tasks/cgt-xxxx... \
  -H "Authorization: Bearer <你的 api_key>"
```

> 图生视频（image_url 参考图）暂不支持（上游附件上传通道未接入），当前仅文生视频。
> 协议依据 Doora v0.8.2 逆向结论（提交 + 轮询 + 自动确认轮 + 无水印选优），上游改版时字段可能变动。

**生成额度（面板「生成额度」页）**：豆包网页端无公开额度接口，采用本地按账号日计数：
图片按张、视频按条、对话按次（提交成功即计），跨日自动清零；达到每日上限的账号自动
冷却并切到下一个豆包账号（多账号分摊额度）。上限在 `config.json`
`doubao.image_daily_limit` / `doubao.video_daily_limit` 配置（默认 50 / 10），
面板展示每个账号的进度条与按模型消耗明细。

## ⏰ 签到排程（多账号自用的关键四条）

一个人管十几个号时，真正的风险不是「签不上」，而是**签得太整齐**：N 个账号在
09:00:00 同时打上游、失败后每分钟重试一次、整点齐发到深夜。本项目把这四件事都做掉了：

| 机制 | 配置项 | 默认 | 说明 |
|---|---|---|---|
| 时点抖动 | `checkin_jitter_minutes` | 15 | 进入配置小时后，每个账号在 0~15 分钟内随机取一个时刻，同一小时只随机一次 |
| 全局并发闸门 | `checkin_concurrency` | 2 | 同一时刻最多 2 个账号在打上游，其余排队（排队上限 2 分钟） |
| 指数退避重试 | `checkin_retry_minutes` | 5/15/60/180 分 | 失败后按序列重试，次数用尽当日不再尝试（不轰炸上游） |
| 当日截止 | `checkin_deadline` | 23:30 | 过点不再发起新尝试，跨日重置计数 |

**签到结果的三种「非失败」语义**（面板与推送都按此着色，避免红色噪音）：

- `claimed`：本次真实领到 → `✓ 领取 09:03`
- `already`：上游判定今日已签（幂等） → `✓ 已签 09:03`
- `info`：平台没有签到体系 / 需额外开启浏览器通道 → 中性蓝标签，**不计失败**，当日继续复查

记录落盘在 `data/state.json` 的 `checkin_log`（键 `provider:uid:date`，值带 `phase` 与 `at`，
保留 30 天）。`at` 不是装饰：部分平台的每日活动并非零点刷新（例如 Qoder 上午 10:00），
只有日期没有时刻时，无法判断一条「已签」是今天的还是昨天残留的。

**推送**：一次签到结算（无在途 + 静默 20 秒）后只发一条汇总，形如

```
签到汇总 11-08 09:12 · 5 个账号
成功领取 3 · 今日已签 1 · 无签到体系 1 · 失败 0
· workbuddy/阿三：+200 签到成功
```

`notify_on_all_success=false` 可改成「只在有异常时推」，这是长期挂机最省心的设置。

**渠道**：填任意一个即可，多个会同时发。面板上点「测试推送」可在 10 秒内验证通不通。

| 渠道 | 填什么 | 备注 |
|---|---|---|
| `wecom` | 企业微信群机器人地址 | 群里「添加群机器人」拿到的 `qyapi.weixin.qq.com/...` 地址，最省事 |
| `feishu` | 飞书群机器人地址 | `open.feishu.cn/open-apis/bot/v2/hook/...` |
| `dingtalk` | 钉钉群机器人地址 | 开了「加签」还需填 `dingtalk_secret` |
| `bark` | Bark 设备 key 或完整地址 | iPhone，App 里复制即可 |
| `webhook` | 任意接受 `{"title","text"}` 的地址 | 粘上面四家的地址进来也行，会按对方格式自动改写 |
| `telegram_token` + `telegram_chat` | Bot token 与 chat_id | 需要能直连 Telegram |
| `serverchan` | Server 酱 SendKey | `SCT` 开头 |

群机器人都有一个共同的坑：**HTTP 200 但响应体 `errcode` 非 0 = 拒收**。这里会读响应体，
拒收时报「企业微信拒收: invalid webhook url（errcode=93000）」，不会假装成功。

## ⚙️ 配置（config.json）

```jsonc
{
  "listen": ":8313",                    // 监听地址
  "api_key": "sk-xxx",                  // /v1 网关密钥
  "panel_password": "123456",           // 面板密码
  "data_dir": "./data",                 // 账号凭证/用量/状态目录
  "gateway_enabled": true,              // 只当代签器用时可设 false：不挂载 /v1，面板与签到照常
  "schedule": {
    "checkin_hours": [9, 21],           // 每日自动签到时刻（小时）
    "checkin_enabled": true,
    "checkin_concurrency": 2,           // 签到全局并发上限（同一时刻最多几个号在打上游）
    "checkin_jitter_minutes": 15,       // 时点抖动：进入该小时后的 0~N 分钟内随机取时刻
    "checkin_deadline": "23:30",        // 当日截止：过点不再发起尝试
    "checkin_retry_minutes": [5, 15, 60, 180],  // 失败退避序列，用尽后当日不再尝试
    "tasks_enabled": true,              // WorkBuddy 任务中心自动完成
    "quota_refresh_minutes": 10,        // 额度自动刷新间隔
    "token_refresh_hours": [3]          // token 保活刷新时刻
  },
  "notify": {                           // 签到结果推送（配置任一渠道即生效，需 enabled=true）
    "enabled": false,
    "webhook": "",                      // 通用：POST {"title","text"}；粘企微/飞书/钉钉/Bark 地址也行
    "wecom": "",                        // 企业微信群机器人地址
    "feishu": "",                       // 飞书群机器人地址
    "dingtalk": "",                     // 钉钉群机器人地址
    "dingtalk_secret": "",              // 钉钉加签密钥（用关键词/IP 白名单可留空）
    "bark": "",                         // Bark 设备 key 或 https://api.day.app/xxx
    "telegram_token": "",               // 与 telegram_chat 同时配才生效
    "telegram_chat": "",
    "serverchan": "",                   // SCT 开头的 SendKey
    "timeout_seconds": 10,
    "notify_on_all_success": true       // false = 只有失败/说明型时才推
  },
  "qwen": { "daily_request_limit": 2000 },
  "doubao": {
    "image_daily_limit": 50,
    "video_daily_limit": 10,
    "checkin_via_browser": false,       // 开启豆包每日签到（每次临时起无头 Chrome）
    "chrome_path": "",                  // 留空自动探测 Chrome/Edge
    "proxy": "direct",                  // ""=跟随环境变量 / "system"=Windows 系统代理
                                        // "direct"=强制直连 / 或填 "http://127.0.0.1:2080"
    "browser_no_proxy": true            // 无头浏览器强制直连（防系统代理残留把签到拖死）
  },
  "minimax": {
    "region": "cn",                     // 默认区域（cn / global），账号自身的 realm 优先
    "timezone": ""                      // 签到结算时区（留空 Asia/Shanghai）
  },
  "pricing": {
    "file": "",                         // 自定义单价表（缺省 data/pricing.json）
    "usd_cny": 7.1                      // 费用估算汇率
  }
}
```

### 自定义计价表（可选，`data/pricing.json`）

```json
{
  "usd_cny": 7.1,
  "models": {
    "qwen3-coder-plus": { "input": 0.002, "output": 0.006 },
    "glm-5.2": { "input": 0.002, "output": 0.008 }
  }
}
```

费用估算 = 输入 tokens/1M × input 价 + 输出 tokens/1M × output 价，再按汇率折人民币。
内置常用国产模型参考价，仅供参考，非计费依据。

## 📁 数据目录

```
data/
├── accounts/            # 账号凭证（workbuddy-*.json / qwen-*.json / trae-*.json）
├── usage/               # 每日用量日志（usage-YYYYMMDD.jsonl）
├── state.json           # 额度快照 + 签到历史
├── models.json          # 发布模型配置（对外模型 → 多渠道 + 单价）
└── pricing.json         # 可选自定义单价表
```

## 🔌 HTTP 接口一览

**网关（Bearer api_key）**

| 路由 | 说明 |
|---|---|
| `GET /v1/models` | 模型列表（配置发布模型后只返回已发布模型） |
| `POST /v1/chat/completions` | 对话（流式/非流式） |
| `POST /v1/images/generations` | 图片生成（OpenAI Images；`/api/v3/images/generations` 同义） |
| `POST /api/v3/contents/generations/tasks` | 视频生成任务·创建（火山方舟 Ark 协议；`/v1/videos` 同义） |
| `GET /api/v3/contents/generations/tasks/{id}` | 视频生成任务·查询（`/v1/videos/{id}` 同义） |

**面板（cookie 会话）**

| 路由 | 说明 |
|---|---|
| `POST /api/auth` | 面板登录 |
| `GET /api/overview` | 仪表板数据（账号/额度/今日用量/费用） |
| `GET /api/usage?days=7` | 用量历史 |
| `GET /api/models?refresh=1` | 模型列表 |
| `POST /api/login/start` | 发起 OAuth（`{provider}`） |
| `GET /api/login/poll` | 轮询登录结果 |
| `POST /api/login/browser` | 豆包「窗口登录」：起一个可见浏览器让用户自己登录（`{provider}`），返回 `{id}` |
| `GET /api/login/browser_poll` | 窗口登录进度（`id`），成功即已纳管 |
| `POST /api/import` | 粘贴凭证导入（`{provider, credential}`） |
| `GET /api/growth_tasks?provider=&uid=` | 成长任务清单与进度（只读） |
| `POST /api/accounts/action` | 账号操作（checkin/quota/refresh_token/toggle） |
| `DELETE /api/accounts` | 删除账号 |
| `POST /api/checkin_all` | 全账号立即签到 |
| `GET /api/checkin_history?days=7` | 最近 N 天签到记录（含 phase/at）+ 今日进度快照 |
| `POST /api/notify_test` | 发一条测试推送，验证 Webhook / Telegram / Server 酱配置（面板「测试推送」按钮同源） |
| `GET/POST /api/publish` | 发布模型配置读写（对外模型 → 多渠道 + 单价） |
| `GET /trae/callback` | Trae 登录回调（浏览器自动跳转） |

## 🧩 架构

```
cmd/chinacpa            网关入口；cmd/rail  Go 版浮动栏（旧）
rail-rs/                Rust 版桌面浮动栏（现役，cargo build --release）
internal/
├── model/              公共类型（账号/额度/模型/登录会话）
├── config/             配置加载与面板热保存
├── store/              凭证/状态/签到历史落盘（原子写）
├── provider/           平台抽象 + 各实现（错误分类 / 风控闸门识别）
│   ├── workbuddy/      腾讯：OAuth、积分、签到、任务中心、模型、对话
│   ├── qwen/           通义：网页会话通道、日限计数、模型、对话
│   ├── trae/           字节：授权回调、ExchangeToken、签到、额度、SOLO SSE→OpenAI
│   ├── qoder/          阿里 Qoder：PAT 导入、额度、对话
│   ├── qwenwork/       千问办公：钉钉扫码、积分池、对话
│   ├── doubao/         豆包网页协议：Cookie 导入、对话、Seedream 图片、Seedance 视频、签到（CDP 浏览器通道）
│   └── minimax/        MiniMax：设备码授权、token 续期、签到（含回查确认）、积分、Anthropic 协议对话转换
├── pool/               多账号池：轮询、冷却、模型路由、风控闸门退避重试
├── notify/             签到结果推送（Webhook / Telegram / Server 酱，无重试、尽力而为）
├── netdiag/            分通路网络诊断（直连 / 环境变量代理 / Windows 系统代理逐跳探测）
├── pubmodel/           发布模型配置（对外模型 → 多渠道 + 单价）
├── scheduler/          定时：签到、任务、额度刷新、保活
├── usage/              用量 JSONL + 聚合 + 计价估算（发布单价优先）
├── reqlog/             bbolt 请求审计日志（按范围聚合：模型×渠道×实例）
├── gateway/            OpenAI / Responses / Claude 三协议网关（SSE 透传、usage 提取、密钥热更换）
├── translator/         协议翻译层（Claude ↔ OpenAI、Responses ↔ OpenAI）
├── thinking/           Claude 思考链配置处理（translator 配套）
├── signature/          Claude thinking signature 校验（translator 配套）
├── registry/           模型注册表垫片（thinking/signature 依赖的最小类型）
├── client/             上游 HTTP 客户端封装
├── panel/              管理 API + 内嵌仪表板（embed 单文件 HTML）
└── util/               杂项工具
```

## ⚠️ 注意

- **本项目仅限个人学习研究使用**：接口逆向、账号纳管、额度反代均为技术探索性质，请勿用于商业用途、转售分发或任何违反各平台服务条款的场景；使用者需自行承担全部风险与后果
- 上游风控收紧时对应通道可能失效；各平台凭证即金钱：`data/` 目录请妥善保管，不要外传
- **成长任务的「补进度」属于伪造客户端遥测**：用事件上报把 `expert_5`、`expert_use_xxx` 之类任务刷到达标再领奖，
  在平台侧是可聚类的特征（几秒内点亮整条专家链、对话只有一个字符、`requestId` 复用）。
  这是本项目里最接近「骗取奖励」的行为，一旦被判定基本没有申诉空间；多账号同机同 IP 还会连带主账号。
  只想长期稳定用的话，建议停用成长任务、保留签到；网关只自用不转售
- 默认 `panel_password` 与 `api_key` 是公开值，**部署后第一件事就是换掉**；`listen` 默认建议只绑 `127.0.0.1`，
  要暴露到局域网务必先改密码，否则同网段任何人都能看到全部账号 Cookie
- WorkBuddy 刷新 token 有防重放（12153），本工具已做单账号串行刷新；本版本另有单实例守卫（端口占用即提示退出），
  但仍不要把同一份 `data/` 复制给两个进程同时用 —— 复制时请连同 `listen` 一起改
- Trae 登录回调要求面板与浏览器同机（127.0.0.1 回调）或局域网可达；服务器部署时可用「粘贴凭证」方式纳管

## 🛠 相对上游的改动

> 基线是 [XingQiPan/ChinaCPA](https://github.com/XingQiPan/ChinaCPA)。下面只列**本仓库额外做的**，
> 未提及的部分保持上游原样。成长任务的事件链与动作表移植自
> [workbuddy2api-trae-qoder-minimax-panel](https://github.com/xu20060420fan-cell/workbuddy2api-trae-qoder-minimax-panel)。

### 面板与交互（`internal/panel/static/index.html`）

| 改动 | 说明 |
|---|---|
| 仪表盘卡片化 | 每张卡 `flex column` + `max-height`，卡头 `flex:none` 固定、`.sec-body` 为唯一滚动容器；`.dc-cap/.dc-model/.dc-chan/.dc-check/.dc-hist/.dc-log` 分别给 344/352/346/340/426/344px 基准高度，内容更短时自动收缩，拓扑卡不设上限 |
| 卡内滚动提示 | `syncCardScroll()` 依据 `scrollHeight > clientHeight` 打 `is-scroll` / `at-bottom`，配合 `.dash-card::after` 做底部羽化；`MutationObserver` 在数据重绘后重新判定 |
| 单卡展开 / 收起 | `initDashCards()` 注入右上角 `.card-toggle`（图标 `#i-expand`，悬停显形、触屏常显半透明），点一下把该卡摊开到全部内容 |
| 排版收敛 | 两栏比例 1.72 : 1、间距 16px；KPI 卡与网关接入条整体压紧；接入条在 1300~1500px 折行时末项不再 `margin-left:auto`（避免折行后出现半行空白）；卡片标题统一小号大写字 + 右侧说明 + 卡头分隔线 |
| 窄屏回落 | ≤900px 取消卡内滚动与展开按钮，回归整页滚动；≤1120px 两栏坍成单栏 |
| 网络诊断弹窗 | 豆包生成额度页「网络诊断」→ `GET /api/diag`，逐跳列出 DNS → TCP → TLS → HTTP 与证书签发者 |
| 成长任务弹窗 | 账号行「成长任务」→ `GET /api/growth_tasks` 只读快照 + `action:"tasks"` 一键完成（超时放宽到 8 分钟，覆盖真实对话链） |
| 窗口登录兜底 | 豆包三处入口都能「打开浏览器窗口登录」：`POST /api/login/browser` + 轮询 `GET /api/login/browser_poll`，带 409 状态与过期清理 |

### 平台适配（`internal/provider/`）

| 改动 | 说明 |
|---|---|
| WorkBuddy 成长任务 | 新增 `taskactions.go`（动作表 + 编排：accept 按默认/小程序口径分开提交 → 补进度 → 有界轮询 → 领奖）、`desktop.go`（桌面/Web 事件链）、`report.go`（CLI `chat_request_send` 活跃上报） |
| WorkBuddy 专家/技能类任务 | 新增 `expert.go`：`jump_url` 解析 `expertId%3Dex_xxx`、一次对话同时点亮 `expert_5` 与 `expert_use_xxx`、`max_tokens=16` + 单字符输入、跑完回读只补差额（实测 19 条任务两轮 +700 积分、耗 6 额度） |
| WorkBuddy 进度解析 | `growthTask` 增 `JumpURL/Desc/Locked/Progress`，`normalize()` 解析线上真实的 `progress` 子对象（不是平铺 `current/target`）；`claimReward` 两级兜底 + web 域降级；`listTasks` 去重 |
| 豆包窗口登录 | 新增 `browserlogin.go`：起独立 user-data-dir 的可见浏览器，每 1.5s 读 Cookie，只留 `*.doubao.com`、同名去重（6 例单测） |
| 豆包链路诊断 | 新增 `internal/netdiag/`（`EnvProxy` / `SystemProxy` / `Check` 逐跳三路探测 + `Summary`）与 `doubao/diag.go`；导入失败时自动改走直连重试一次 |
| MiniMax | 新增 `internal/provider/minimax/`：设备码授权（RFC 8628 + PKCE）、token 续期、签到「查状态 → 领取 → 回查确认」、Anthropic 协议对话转换 |

### 运行时与工程

| 改动 | 说明 |
|---|---|
| 单实例守卫（`cmd/chinacpa/main.go`） | `net.Listen` 提到最前：端口被占时 1 秒内中文报错并退出，不再两个进程抢 bbolt 锁；bbolt 加锁超时也补提示；启动打印 exe 构建时间 |
| `start.bat` | 改为**先结束旧进程 → 每次都重新编译 → 无 Go 时才用现有 exe**；端口占用给中文提示（旧版只在 exe 缺失时编译，导致改了代码跑的还是旧进程） |
| 打包（`build_dist.sh`） | 一次交叉编译三平台，产出 `dist/` 下 zip + 两个 tar.gz + `SHA256SUMS.txt`；`CGO_ENABLED=0 -trimpath -ldflags "-s -w"` 得纯静态单文件；用 Python `tarfile` / `zipfile` 显式写权限位（Windows 上 `chmod` 不落盘） |
| Linux 运行（`packaging/`） | `start.sh`（前台 / `-d` 后台，面板密码从 config 读而不是写死）+ `chinacpa.service`（systemd 单元） |
| 打包默认配置 | 发行包内 `listen` 收敛为 `127.0.0.1:8313`，避免默认口令 + 默认密钥直接暴露到局域网 |
| 推送与审计 | 新增 `internal/notify/`（多渠道路由、一次结算一条汇总）与 bbolt 请求审计聚合 |
| 后台预算 | 任务中心独立预算 100s → 300s，面板成长任务请求 4 → 8 分钟（真实对话链会超过旧超时被截断） |

## 🙏 参考

本仓库为**二创（衍生）版本**，在两份上游成果之上继续开发：

- **[XingQiPan/ChinaCPA](https://github.com/XingQiPan/ChinaCPA)** —— 基础仓库：网关三协议、账号池与路由、
  面板框架、WorkBuddy / Qwen / Trae / Qoder / 千问办公 / 豆包 / MiniMax 的平台适配主体，
  以及此前已有的风控闸门退避重试、服务密钥热更换、用量范围查询、请求审计聚合等改造。
- **[xu20060420fan-cell/workbuddy2api-trae-qoder-minimax-panel](https://github.com/xu20060420fan-cell/workbuddy2api-trae-qoder-minimax-panel)**
  —— WorkBuddy **成长任务**的事件指纹上报（桌面 / CLI / Web 三通道）与动作链思路，
  以及 Trae / Qoder / MiniMax 面板的字段口径；本仓库的 `taskactions.go`、`desktop.go`、
  `report.go`、`expert.go` 是在其基础上重写为 Go 实现，并补了「一条对话点亮多条任务」的低消耗方案。

接口口径来自以下开源项目的实测成果：

- [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（WorkBuddy 全套）
- [connectedGraph/trae2api-web](https://github.com/connectedGraph/trae2api-web)（Trae SOLO 通道）
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（Qwen OAuth，v6.0.0 实现）
- [ardeyouxipianyi/workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub)（计价/用量思路）

桌面浮动栏（`rail-rs/`）的悬浮交互与视觉设计参考自：

- [qunqin24/Pulse](https://github.com/qunqin24/Pulse)（macOS 屏幕边缘 AI 用量监视器，Apache-2.0；本项目以 Rust + Direct2D 为其 Windows 复刻实现）

## License

MIT

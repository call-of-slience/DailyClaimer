// expert.go 专家/技能类成长任务的「最小消耗」动作链。
//
// 这类任务（expert_5 / Expert_team_use_3 / expert_use_* / WorkBuddy_Valorant /
// skill_1）与纯事件任务不同：判据里的 requestId 必须是**服务端真实对话**返回的 id，
// 自造 UUID 一律不计数（上游 expert_actual_use 会 JOIN 服务端请求记录）。
// 所以必须真的发一次 chat —— 但可以发得极小。
//
// 省 token 的三个手段：
//  1. 合并计数：一次「召唤 + 真实对话 + 使用事件」同时给 expert_5 计 1 次，
//     并点亮对应的 expert_use_xxx。5 条具名任务 + expert_5 只需 5 次对话，
//     不是 5+5=10 次。
//  2. 压缩请求：不带 system prompt，用户消息只发 `1`，max_tokens=16。
//     单次对话约 20 输入 + 8 输出 token，8 次合计不到 300 token。
//  3. 自适应补差：先跑具名任务，再回读真实进度决定 expert_5 / 团队还差几次，
//     已达标就不再打。
package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"chinacpa/internal/model"
)

// serverReqID 服务端 requestId 形状：cmb- 前缀 + 32 hex，或裸 32 hex。
var serverReqID = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// minChatMaxTokens 单次对话的输出上限。够模型回一个短句，不至于 400。
const minChatMaxTokens = 16

// expertMinPrompt 最小输入。别改长：每个字都是钱。
const expertMinPrompt = "1"

// expertChainGap 两次「召唤 + 对话」之间的间隔，模拟真人节奏，防频控。
var expertChainGap = 3 * time.Second

// marketExpert 专家市场条目（/portal/operation-platform/market/expert/list 子集）。
type marketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// marketExpertList 拉专家市场真实列表。判据校验要求 id 真实存在，编的不算。
func (p *Provider) marketExpertList(ctx context.Context, a *model.Account, expertType, keyword string, page, pageSize int) ([]marketExpert, error) {
	body := map[string]any{"page": page, "page_size": pageSize, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	// keyword 是服务端唯一认的搜索字段（search/q/name 都会被忽略，返回全量）。
	if keyword != "" {
		body["keyword"] = keyword
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		chatBase(a)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []marketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("专家列表解析失败: %w", err)
	}
	return out.Experts, nil
}

// desktopChatMinimal 发一次**尽可能小**的真实对话，返回服务端 requestId。
// 只发一个字符、限制输出长度，但让流正常走完（中途断开可能被判无效请求）。
func (p *Provider) desktopChatMinimal(ctx context.Context, a *model.Account, expertID string) (conversationID, requestID string, err error) {
	conversationID = fmt.Sprintf("ccpa-econv-%d", time.Now().UnixNano())
	body := map[string]any{
		"model":          "fast-model",
		"messages":       []any{map[string]any{"role": "user", "content": expertMinPrompt}},
		"temperature":    1,
		"max_tokens":     minChatMaxTokens,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatBase(a)+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", chatBase(a))
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", a.UID)
	h.Set("X-Conversation-ID", conversationID)
	h.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", "5.5.6")
	h.Set("x-codebuddy-request", "1")
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
	// 长流：给足 5 分钟，别用 Provider 默认的 60s（大模型排队会超）
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("对话 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(head)))
	}
	// 从 SSE 抓第一个形如服务端 id 的字段
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 4096)
	searchFrom := 0
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if serverReqID.MatchString(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1
				}
			}
		}
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return "", "", fmt.Errorf("流里没有服务端 requestId（该次对话未计入，稍后重试）")
}

// desktopExpertSummon 「召唤平台专家」事件组（顺序与字段对齐客户端抓包）。
func desktopExpertSummon(e marketExpert) []desktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []desktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// desktopExpertActualUseEvent 「专家真实使用」事件（expert_5 / 具名 expert_use_* 计数）。
// requestID 必须来自 desktopChatMinimal。
func desktopExpertActualUseEvent(e marketExpert, conversationID, requestID string) desktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return desktopEvent{
		"eventCode": "expert_actual_use", "id": e.ExpertID, "name": e.DisplayNameZH,
		"expertTitle": e.ProfessionZH, "expertType": e.ExpertType, "type": cat,
		"version": ver, "mode": "craft", "cost": 1000,
		"conversationId": conversationID, "requestId": requestID,
		"messageId": "msg-" + tail8(requestID),
	}
}

// desktopSkillInfoEvent 技能加载事件（skill_1 判据）。
func desktopSkillInfoEvent(conversationID, requestID string) desktopEvent {
	return desktopEvent{
		"eventCode":        "skill_info",
		"id":               "润泽小馆·日报撰写",
		"skillId":          "skill_2097350077599879168",
		"skillVersion":     "1.0.0",
		"toolStatus":       "success",
		"fileCount":        56,
		"source":           "workbuddy-desktop",
		"conversationId":   conversationID,
		"requestId":        requestID,
		"messageId":        "msg-" + tail8(requestID),
		"requestModelId":   "fast-model",
		"requestModelName": "fast-model",
		"traceId":          requestID,
	}
}

// tail8 取末 8 位（messageId 后缀口径）。
func tail8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[len(s)-8:]
}

// mpEventBase 小程序埋点公共指纹（与客户端 appservice 对齐）。
func mpEventBase(a *model.Account) map[string]any {
	return map[string]any{
		"timestamp": time.Now().UnixMilli(),
		"ideType":   "WorkBuddy_MP", "ideVersion": "2.4.0",
		"extName": "workbuddy-mp", "extVersion": "2.4.0",
		"product": "SaaS", "ideName": "wx_app_cloud", "platform": "mini_program",
		"os": "windows", "osVersion": "11", "arch": "x64",
		"machineId":    stableID(a.UID, "mp-machine"),
		"timezone":     "Asia/Shanghai",
		"userId":       a.UID,
		"userNickname": a.Nickname,
	}
}

// reportMPEvents 以小程序指纹批量上报（billing 域 /v2/report）。
func (p *Provider) reportMPEvents(ctx context.Context, a *model.Account, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("小程序事件为空")
	}
	base := mpEventBase(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(base)+len(ev))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, billingBase(a)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = p.doJSON(req)
	return err
}

// reportMPChatEvent 小程序口径对话事件（Sequential_Tasks_1 判据，零 token）。
func (p *Provider) reportMPChatEvent(ctx context.Context, a *model.Account, conversationID string) error {
	rid := fmt.Sprintf("ccpa-mp-%d", time.Now().UnixNano())
	return p.reportMPEvents(ctx, a, map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + tail8(rid),
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	})
}

// miniExpertUseEvent 小程序口径「选中专家并对话」事件（Sequential_Tasks_2 判据）。
// 真实事件不带 conversationId/activityId，extVersion 用小程序自身 2.2.8，
// type 恒为 send_message —— 这三处照抄 PC 口径就会不计数。
func miniExpertUseEvent(e marketExpert) map[string]any {
	name := e.DisplayNameZH
	if name == "" {
		name = e.ExpertID
	}
	typ := e.ExpertType
	if typ == "" {
		typ = "agent"
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": e.ExpertID, "name": e.ExpertID,
		"expertTitle": name, "type": "send_message",
		"characterCount": 12, "expertType": typ,
	}
}

// runExpertChain 一次完整「召唤 → 真实对话 → 使用事件」。
func (p *Provider) runExpertChain(ctx context.Context, a *model.Account, e marketExpert) error {
	if err := p.reportDesktop(ctx, a, desktopExpertSummon(e)...); err != nil {
		return fmt.Errorf("召唤链上报失败: %w", err)
	}
	conv, reqID, err := p.desktopChatMinimal(ctx, a, e.ExpertID)
	if err != nil {
		return err
	}
	events := desktopChatSequence(conv, reqID, "msg-"+tail8(reqID), "fast-model", "fast-model")
	events = append(events, desktopExpertActualUseEvent(e, conv, reqID))
	if err := p.reportDesktop(ctx, a, events...); err != nil {
		return fmt.Errorf("使用事件上报失败: %w", err)
	}
	return nil
}

// namedExpertTargets 具名专家任务 → 名称关键词（用于在市场列表里定位真实 id）。
// 关键词别写太长：市场里的展示名带后缀，包含匹配更稳。
var namedExpertTargets = map[string]string{
	"WorkBuddy_Valorant":            "无畏契约",
	"expert_use_maidehao":           "电商",
	"expert_use_douyinyin":          "抖音",
	"expert_use_shangjia_xuanchuan": "海报",
	"expert_use_bowangwang":         "直播",
}

// pickExpert 按关键词在列表里挑一个还没用过的专家。
func pickExpert(list []marketExpert, kw string, used map[string]bool) *marketExpert {
	for i := range list {
		e := &list[i]
		if e.ExpertID == "" || used[e.ExpertID] {
			continue
		}
		if strings.Contains(e.DisplayNameZH, kw) || strings.Contains(e.ProfessionZH, kw) {
			return e
		}
	}
	return nil
}

// expertIDFromJumpRe 任务跳转链接里的专家 id。
// 注意 deeplink 是 URL 编码过的：`workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_xxx`，
// 分隔符可能是 `=` 也可能是 `%3D`，两种都要认（只认 `=` 会静默回退到关键词搜索，
// 搜出来的是另一个人，任务照样不点亮）。
var expertIDFromJumpRe = regexp.MustCompile(`expertId(?:=|%3D)(ex_[0-9A-Za-z]+)`)

// expertIDFromJump 取 jump_url 里的 expertId，取不到返回空串。
func expertIDFromJump(u string) string {
	if m := expertIDFromJumpRe.FindStringSubmatch(u); len(m) == 2 {
		return m[1]
	}
	return ""
}

// titleKeyword 取标题里「」中的名字（`体验「抖音策略师」` → `抖音策略师`）。
func titleKeyword(title string) string {
	i := strings.Index(title, "「")
	if i < 0 {
		return ""
	}
	rest := title[i+len("「"):]
	j := strings.Index(rest, "」")
	if j <= 0 {
		return ""
	}
	return rest[:j]
}

// resolveTaskExpert 定位具名任务要召唤的专家。
//
// 优先级：jump_url 的 expertId（任务元数据给的，最准）→ 按标题关键词搜市场 →
// 两者都没有则报错。只靠关键词猜过：市场里 4 万多个专家，抖音策略师这类
// 活动专家根本不在推荐列表前 600 条里，猜不中。
func (p *Provider) resolveTaskExpert(ctx context.Context, a *model.Account, t *growthTask) (*marketExpert, error) {
	if t == nil {
		return nil, fmt.Errorf("任务不在列表里")
	}
	id := expertIDFromJump(t.JumpURL)
	name := titleKeyword(t.Title)
	var list []marketExpert
	if name != "" {
		if l, err := p.marketExpertList(ctx, a, "agent", name, 1, 20); err == nil {
			list = l
		}
	}
	for i := range list {
		if id != "" && list[i].ExpertID == id {
			return &list[i], nil
		}
	}
	if id == "" && len(list) > 0 {
		return &list[0], nil
	}
	if id != "" {
		// 搜不到完整记录也无妨：id 来自任务元数据，服务端认可；展示字段用标题补齐。
		if name == "" {
			name = strings.TrimSpace(t.Title)
		}
		if name == "" {
			name = id
		}
		return &marketExpert{
			ExpertID: id, ExpertType: "agent",
			DisplayNameZH: name, ProfessionZH: name, Version: "1.0.0",
		}, nil
	}
	return nil, fmt.Errorf("任务没给专家 id，按「%s」也没搜到", name)
}

// progressOf 回读单个任务的 current/target（读不到按 0 处理）。
func progressOf(tasks []growthTask, code string) (cur, tgt int64) {
	if t := findTask(tasks, code); t != nil {
		return t.Current, t.Target
	}
	return 0, 0
}

// needOf 还差几次；target 缺失时用 def。
func needOf(tasks []growthTask, code string, def int64) int64 {
	cur, tgt := progressOf(tasks, code)
	if tgt <= 0 {
		tgt = def
	}
	n := tgt - cur
	if n < 0 {
		n = 0
	}
	return n
}

// runExpertGroup 专家类任务合并执行（一条链点亮多条任务，省 token）。
// 返回逐条摘要，以及已处理的任务码（调用方据此跳过单任务动作）。
func (p *Provider) runExpertGroup(ctx context.Context, a *model.Account, tasks []growthTask) ([]string, map[string]bool) {
	var parts []string
	handled := map[string]bool{}

	// 需要跑的先筛出来：已领 / 已达标都不用动
	var named []string
	for code := range namedExpertTargets {
		t := findTask(tasks, code)
		if t == nil || t.AcceptStatus == "claimed" || t.Status == "claimed" || t.Locked || isClaimable(*t) {
			continue
		}
		named = append(named, code)
	}
	needFive := needOf(tasks, "expert_5", 5)
	needTeam := needOf(tasks, "Expert_team_use_3", 3)
	if len(named) == 0 && needFive == 0 && needTeam == 0 {
		return nil, handled
	}

	// 专家池只给 expert_5 补量用；具名任务各自按 id/关键词单独定位，互不依赖。
	pool, perr := p.marketExpertList(ctx, a, "agent", "", 1, 50)
	if perr != nil {
		parts = append(parts, "专家池拉取失败（通用专家任务本轮跳过）："+perr.Error())
	}
	used := map[string]bool{}
	done := 0

	// ① 具名任务：一次链同时给 expert_5 计 1 次，所以先跑它们。
	for _, code := range named {
		if ctx.Err() != nil {
			break
		}
		t := findTask(tasks, code)
		e, rerr := p.resolveTaskExpert(ctx, a, t)
		if rerr != nil {
			parts = append(parts, fmt.Sprintf("%s 未定位到专家：%v", code, rerr))
			handled[code] = true
			continue
		}
		if err := p.runExpertChain(ctx, a, *e); err != nil {
			parts = append(parts, fmt.Sprintf("%s 失败：%v", code, err))
			handled[code] = true
			continue
		}
		used[e.ExpertID] = true
		done++
		handled[code] = true
		parts = append(parts, fmt.Sprintf("%s 已召唤「%s」", code, e.DisplayNameZH))
		sleepCtx(ctx, expertChainGap)
	}

	// ② expert_5：回读真实进度再补差，具名链若已计入就不重复打。
	if needFive > 0 && ctx.Err() == nil {
		if ts, err := p.listTasks(ctx, a); err == nil {
			needFive = needOf(ts, "expert_5", 5)
		}
		for i := int64(0); i < needFive; i++ {
			if ctx.Err() != nil {
				break
			}
			e := pickExpert(pool, "", used)
			if e == nil {
				break
			}
			if err := p.runExpertChain(ctx, a, *e); err != nil {
				parts = append(parts, "召唤专家失败："+err.Error())
				break
			}
			used[e.ExpertID] = true
			done++
			sleepCtx(ctx, expertChainGap)
		}
		handled["expert_5"] = true
		if done > 0 {
			parts = append(parts, fmt.Sprintf("召唤并对话 %d 位专家", done))
		}
	}

	// ③ 专家团：另一类 id（expert_type=team），单独拉列表。
	if needTeam > 0 && ctx.Err() == nil {
		if ts, err := p.listTasks(ctx, a); err == nil {
			needTeam = needOf(ts, "Expert_team_use_3", 3)
		}
		teams, terr := p.marketExpertList(ctx, a, "team", "", 1, 20)
		if terr != nil {
			parts = append(parts, "专家团列表拉取失败："+terr.Error())
		} else {
			teamsUsed := map[string]bool{}
			okTeam := 0
			for i := int64(0); i < needTeam; i++ {
				if ctx.Err() != nil {
					break
				}
				e := pickExpert(teams, "", teamsUsed)
				if e == nil {
					break
				}
				if err := p.runExpertChain(ctx, a, *e); err != nil {
					parts = append(parts, "召唤专家团失败："+err.Error())
					break
				}
				teamsUsed[e.ExpertID] = true
				okTeam++
				sleepCtx(ctx, expertChainGap)
			}
			if okTeam > 0 {
				parts = append(parts, fmt.Sprintf("召唤并对话 %d 个专家团", okTeam))
			}
		}
		handled["Expert_team_use_3"] = true
	}
	return parts, handled
}

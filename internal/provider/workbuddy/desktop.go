// desktop.go 桌面/Web 客户端行为指纹上报——「完成」成长任务的关键手段。
//
// 背景：WorkBuddy 的成长任务（任务中心）里，大部分任务的判据不是「点一下按钮」，
// 而是客户端上报的行为事件。只调 accept + claim 只能领到「已达标」的任务奖励，
// 无法把 current 从 0 推到 target。本文件把客户端的事件形状搬过来，用同一批
// 上报通道（POST /v2/report）补足进度。
//
// 三条通道，指纹各不相同，不能混用：
//
//	桌面端  POST https://copilot.tencent.com/v2/report
//	        UA: WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1
//	        必带 extName=workbuddy-desktop 等指纹 → RichMeow_Chat / Buddy_App / 模板 / 灵感 / 画布
//	CLI 端  POST https://www.codebuddy.cn/v2/report（billing 域）
//	        事件 chat_request_send，必带 userId → chat_5 进度、first_buddy 解锁前置
//	Web 端  POST https://www.workbuddy.cn/v2/report
//	        浏览器形状（os/machineId/userAgent）→ Library_read 等页面行为任务
//
// 来源：上游项目对客户端的事件逆向与多账号实测结论，各构造函数的注释保留了
// 「哪条事件点亮哪个任务」的经验，改形状前先看注释。
package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"chinacpa/internal/model"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// desktopUA 桌面客户端实测 UA（5.5.6 内嵌 CLI 2.137.1）。
	// 注意与 p.commonHeaders 里的 UA 不同：成长任务的行为链按桌面端口径校验。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
)

// desktopEvent 桌面端事件：业务字段自由，公共指纹由 reportDesktop 注入。
type desktopEvent map[string]any

// siteBase 站点域（workbuddy.cn / workbuddy.ai），Web 端上报与页面类链接用。
func siteBase(a *model.Account) string {
	if isGlobal(a) {
		return globalBase
	}
	return webBaseCN
}

// desktopFingerprint 桌面端公共指纹（注入每个事件，业务字段可覆盖同名键）。
func (p *Provider) desktopFingerprint(a *model.Account) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       a.UID,
		"username":     a.Nickname,
		"userNickname": a.Nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    stableID(a.UID, "desktop-machine"),
		"sessionId":    stableID(a.UID, "desktop-session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// reportDesktop 以桌面端指纹批量上报事件（数组报文）。
func (p *Provider) reportDesktop(ctx context.Context, a *model.Account, events ...desktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("上报事件为空")
	}
	fp := p.desktopFingerprint(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(fp)+len(ev))
		for k, v := range fp {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", stableID(a.UID, "desktop-req")+fmt.Sprintf("%d", time.Now().UnixNano()%1e6))
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = p.doJSON(req)
	return err
}

// reportWebEvent 以 Web 端指纹上报单事件（浏览器形状）。
// 与桌面指纹不同：这条通道用的是 workbuddy.cn 域 + 浏览器 UA，
// Library_read 这类「读页面」任务只认这里的 web_element_click。
func (p *Provider) reportWebEvent(ctx context.Context, a *model.Account, eventCode, pageURL, elementID, elementName string) error {
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": stableID(a.UID, "web-machine"), "userId": a.UID,
		"userNickname": a.Nickname, "enterpriseId": a.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, siteBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", siteBase(a))
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", ua)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = p.doJSON(req)
	return err
}

// setAppearanceTheme 设置外观主题（纯 API 设置本身不计 Hp_Appearance 分，
// 需客户端在主题下活跃；保留用于调色与后续验证）。
func (p *Provider) setAppearanceTheme(ctx context.Context, a *model.Account, resourceKey string) error {
	raw, err := json.Marshal(map[string]string{"kind": "theme", "resource_key": resourceKey})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatBase(a)+desktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = p.doJSON(req)
	return err
}

// ── 事件链构造 ───────────────────────────────────────────

// desktopChatSequence 一次「桌面端成功对话」的完整事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// RichMeow_Chat（桌面端对话 1 次）就是被这条链点亮的。
func desktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []desktopEvent {
	uuid := func() string { return requestID }
	mk := func(code string, extra map[string]any) desktopEvent {
		ev := desktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []desktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": uuid(),
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// desktopBuddyAppSequence 「进入 Buddy 应用」五连事件
// （discover → show → enter_click → auth_confirm → bindaccount_skip）。
// buddyID 固定用企鹅教师助手：它同时是 Buddy_App_QQ 的判据应用，也满足
// Buddy_App「进入任一应用」，一组事件覆盖两个任务。
func desktopBuddyAppSequence(buddyID, buddyName string) []desktopEvent {
	mk := func(code string, extra map[string]any) desktopEvent {
		ev := desktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []desktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// desktopAutomationCreateEvent 「定时任务创建成功」事件（automation_1 判据）。
func desktopAutomationCreateEvent(name string) desktopEvent {
	return desktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// desktopTemplateUseSequence 「使用模板创建任务」事件组（template_5 计数）：
// 一条完整 chat 链 + agent_task_created_with_template + template_used。
func desktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []desktopEvent {
	events := desktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		desktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		desktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// desktopPlaybookPromptSequence 「灵感案例做同款」事件组（playbook_prompt 计数）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send。
func desktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []desktopEvent {
	events := desktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	pb := func(code string, extra map[string]any) desktopEvent {
		m := desktopEvent{"eventCode": code}
		for k, v := range payload {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	return append(events,
		desktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		pb("playbook_cta_click", map[string]any{"source": "discover", "position": 0}),
		pb("playbook_prompt_send", map[string]any{"conversationId": conversationID, "requestId": requestID}),
	)
}

// desktopDesignCanvasSequence 「设计创意画布」事件组（create_canvas 计数）：
// chat 链 + wbx_design_canvas_task_create + wbx_design_canvas_open。
func desktopDesignCanvasSequence(conversationID, requestID string) []desktopEvent {
	events := desktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return append(events,
		desktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		desktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + suffix,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

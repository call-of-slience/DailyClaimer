// report.go CLI 端「对话活跃上报」：POST {billingBase}/v2/report。
//
// 与桌面端上报是两条通道、两种指纹：
//   - 桌面端（desktop.go）走 copilot.tencent.com，用来点亮「需电脑客户端」类任务；
//   - CLI 端（本文件）走 codebuddy.cn/billing 域，事件形状是 chat_request_send，
//     点亮的是 chat_5「完成 5 次对话」的进度，同时解锁 first_buddy 的领养前置。
//
// 关键约束（来自上游实测，改之前先看）：
//   - 事件必须带 userId（= 账号 uid）。缺了服务端照样 200，但会**静默丢弃**，
//     表现为「上报成功、进度不动」，极难排查。
//   - 事件形状要完整，不要精简成三个字段：上游后续加严时，缺字段的会被判无效。
//   - 一条上报 = 一次活跃，每天 1 条就够连登；这里是按任务缺口补量，因此会连发。
package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"chinacpa/internal/model"
)

// chatRequestEvent 上游 chat_request_send 事件（字段名与客户端抓包一致，勿改名）。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// reportChatActivity 上报一条对话活跃（默认模型口径）。
func (p *Provider) reportChatActivity(ctx context.Context, a *model.Account, conversationID, requestID string) error {
	return p.reportChatActivityModel(ctx, a, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// reportChatActivityModel 同上，可指定模型：做「体验某模型」类任务时，
// 上报里的 requestModelId 必须与任务要求的模型一致，否则进度不动。
func (p *Provider) reportChatActivityModel(ctx context.Context, a *model.Account, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		ReportDelay:          0,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       modelID,
		RequestModelName:     modelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        requestID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, billingBase(a)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	p.billingHeaders(req, a)
	_, err = p.doJSON(req)
	return err
}

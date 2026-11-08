// taskactions.go 成长任务「完成」动作表。
//
// 只做 accept + claim 是不够的：任务中心里绝大多数任务的进度（current/target）
// 由客户端行为事件推进，光报名不做事，current 永远是 0，奖励也就永远领不到。
// 本文件给每个可自动化的任务码配一条动作链，RunTasks 负责按顺序编排：
//
//	列表 → 接受（两种口径分开提交）→ 有动作且未达标的跑动作 → 等计分结算 → 领奖
//
// 动作表收录「零 token」的纯事件任务，以及「必须真实对话但压到最小」的专家/技能类
// 任务（expert.go：一次对话同时点亮多条，单次约 20 输入 + 8 输出 token）。
// 仍未收录：wb_wechat_oa_subscribe_task（要真的关注公众号，无法代做）。
// 未收录的任务仍会被正常 accept + claim，只是不会自动补进度。
package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chinacpa/internal/model"
)

// reportGap 连续上报之间的间隔。客户端实测节奏约 1.05s，连发会被风控判成脚本。
var reportGap = 1050 * time.Millisecond

// mpTaskCodes 小程序口径专属任务：默认（不带 mp 头）的任务列表里不出现，
// accept/claim 都必须带 X-Client-Platform: miniprogram。新任务出现时在此登记。
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// isMPTask 任务是否小程序口径专属（决定回读/接受/领奖走 mp 变体）。
func isMPTask(code string) bool { return mpTaskCodes[code] }

// mpSafeActions 动作链本身就走小程序口径（mp 指纹上报）的任务。
// 不在这个集合里的小程序码不允许进动作表：PC 事件的指纹不会被 mp 任务计数，
// 混进去只会「上报成功、进度不动」，比不做还难排查。
var mpSafeActions = map[string]bool{
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
}

// taskAction 一个可自动化的任务动作。
type taskAction struct {
	code string
	desc string
	run  func(ctx context.Context, p *Provider, a *model.Account) (string, error)
}

// taskActions 动作表。顺序即执行顺序：带前置依赖的排在前面
// （first_buddy 依赖当日活跃上报，action 内部会自带补上报）。
var taskActions = []taskAction{
	{code: "chat_5", desc: "补报对话活跃事件至目标次数", run: runChat5},
	{code: "first_buddy", desc: "活跃上报 → 同意协议 → 领取第一只 Buddy", run: runFirstBuddy},
	{code: "RichMeow_Chat", desc: "按桌面端指纹上报一次完整对话事件链", run: runRichMeowChat},
	{code: "Buddy_App", desc: "上报「进入 Buddy 应用」五连事件", run: runBuddyApp},
	{code: "Buddy_App_QQ", desc: "上报「进入企鹅教师助手」五连事件", run: runBuddyApp},
	{code: "automation_1", desc: "上报「定时任务创建成功」事件", run: runAutomationCreate},
	{code: "Library_read", desc: "上报「读资料库介绍」页面事件", run: runLibraryRead},
	{code: "template_5", desc: "上报「使用模板创建任务」事件组至目标次数", run: runTemplateUse},
	{code: "playbook_prompt", desc: "上报「灵感案例做同款」事件组", run: runPlaybookPrompt},
	{code: "create_canvas", desc: "上报「设计创意画布」事件组", run: runDesignCanvas},
	{code: "skill_1", desc: "一次极短真实对话 + 技能加载事件", run: runSkillFresh},
	{code: "Sequential_Tasks_1", desc: "上报小程序口径首条对话事件（零 token）", run: runSequentialChat1},
	{code: "Sequential_Tasks_2", desc: "上报小程序口径「选中专家对话」事件（零 token）", run: runMPMiniExpert},
}

// actionFor 查任务对应的动作。
func actionFor(code string) (taskAction, bool) {
	for _, act := range taskActions {
		if act.code == code {
			return act, true
		}
	}
	return taskAction{}, false
}

// findTask 在任务列表里按码定位。
func findTask(tasks []growthTask, code string) *growthTask {
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i]
		}
	}
	return nil
}

// sleepCtx 可被 ctx 打断的等待（不打断的话，取消后还要白等几十秒）。
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ── 各任务的动作 ─────────────────────────────────────────

// runChat5 按差额补报 chat_request_send（chat_5「完成 5 次对话」）。
func runChat5(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return "", err
	}
	t := findTask(tasks, "chat_5")
	if t == nil {
		return "", fmt.Errorf("任务不在列表里")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标", nil
	}
	// 单轮最多补 10 条：目标异常大时不要把一次签到拖成几分钟
	if need > 10 {
		need = 10
	}
	for i := int64(0); i < need; i++ {
		if ctx.Err() != nil {
			return fmt.Sprintf("补报 %d 条后中断", i), nil
		}
		id := fmt.Sprintf("ccpa-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := p.reportChatActivity(ctx, a, id, id); err != nil {
			return fmt.Sprintf("第 %d/%d 条上报失败：%v", i+1, need, err), nil
		}
		if i < need-1 {
			sleepCtx(ctx, reportGap)
		}
	}
	return fmt.Sprintf("补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养第一只 Buddy：先补一条活跃（解锁前置）→ 同意协议 → 领养。
func runFirstBuddy(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	if err := p.reportChatActivity(ctx, a, fmt.Sprintf("ccpa-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置活跃上报失败：%w", err)
	}
	sleepCtx(ctx, reportGap)
	if _, err := p.growthJSON(ctx, a, http.MethodPost, "/v2/activity/growth/buddy/agreement",
		map[string]any{"agree": true}, false); err != nil {
		return "", fmt.Errorf("同意协议失败：%w", err)
	}
	if _, err := p.growthJSON(ctx, a, http.MethodPost, "/v2/activity/growth/buddy/first",
		map[string]any{}, false); err != nil {
		// 已有 Buddy / 门槛未过都是预期分支，不算失败
		return "前置已上报，领养未成功（可能已有 Buddy 或当日活跃门槛未过）", nil
	}
	return "已领取 Buddy", nil
}

// runRichMeowChat 按桌面端指纹上报一次成功对话的完整事件链（RichMeow_Chat）。
func runRichMeowChat(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("ccpa-rm-%d", ms)
	req := fmt.Sprintf("ccpa-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	if err := p.reportDesktop(ctx, a, desktopChatSequence(conv, req, msg, "fast-model", "fast-model")...); err != nil {
		return "", err
	}
	return "已上报桌面端完整对话事件链", nil
}

// runBuddyApp 「进入 Buddy 应用」：一组事件同时满足 Buddy_App 与 Buddy_App_QQ
// （载体用企鹅教师助手，它正是 Buddy_App_QQ 的判据应用）。
func runBuddyApp(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	if err := p.reportDesktop(ctx, a, desktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")...); err != nil {
		return "", err
	}
	return "已上报 Buddy 应用进入事件链", nil
}

// runAutomationCreate automation_1「设置自动化任务」。
func runAutomationCreate(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	if err := p.reportDesktop(ctx, a, desktopAutomationCreateEvent("ChinaCPA 每日签到")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead Library_read「体验资料库」：Web 域 web_element_click。
func runLibraryRead(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.reportWebEvent(ctx, a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库阅读事件", nil
}

// runTemplateUse template_5「使用模板创建任务」：按差额上报事件组。
func runTemplateUse(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return "", err
	}
	t := findTask(tasks, "template_5")
	if t == nil {
		return "", fmt.Errorf("任务不在列表里")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标", nil
	}
	if need > 10 {
		need = 10
	}
	ms := time.Now().UnixMilli()
	for i := int64(0); i < need; i++ {
		if ctx.Err() != nil {
			return fmt.Sprintf("上报 %d 组后中断", i), nil
		}
		conv := fmt.Sprintf("ccpa-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("ccpa-tpl-req-%d-%d", ms, i)
		// 模板 id 每轮不同：同一个模板重复使用不计新的一次
		if err := p.reportDesktop(ctx, a,
			desktopTemplateUseSequence(conv, req, fmt.Sprintf("ccpa-tpl-id-%d", i), "周报模板")...); err != nil {
			return fmt.Sprintf("第 %d/%d 组上报失败：%v", i+1, need, err), nil
		}
		if i < need-1 {
			sleepCtx(ctx, reportGap)
		}
	}
	return fmt.Sprintf("补报 %d 组模板使用事件", need), nil
}

// runPlaybookPrompt playbook_prompt「灵感案例做同款」。
func runPlaybookPrompt(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("ccpa-pb-%d", ms)
	req := fmt.Sprintf("ccpa-pb-req-%d", ms)
	if err := p.reportDesktop(ctx, a,
		desktopPlaybookPromptSequence(conv, req, fmt.Sprintf("ccpa-case-%d", ms), "写作灵感")...); err != nil {
		return "", err
	}
	return "已上报灵感案例事件组", nil
}

// runDesignCanvas create_canvas「设计创意画布」。
func runDesignCanvas(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("ccpa-cv-%d", ms)
	req := fmt.Sprintf("ccpa-cv-req-%d", ms)
	if err := p.reportDesktop(ctx, a, desktopDesignCanvasSequence(conv, req)...); err != nil {
		return "", err
	}
	return "已上报设计画布事件组", nil
}

// runMPMiniExpert Sequential_Tasks_2「在小程序内选中专家并完成有效对话」。
// 判据是小程序口径的 expert_actual_use，纯事件上报，零 token。
// 注意它只在 Sequential_Tasks_1 完成后才下发（连续任务链），所以新账号首轮
// 跑不到它，第二轮才会出现。
func runMPMiniExpert(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return "", err
	}
	t := findTask(tasks, "Sequential_Tasks_2")
	if t == nil {
		return "小程序口径未下发该任务", nil
	}
	if t.AcceptStatus == "claimed" || t.Status == "claimed" {
		return "已领取", nil
	}
	// 专家 id 必须是市场真实条目（空 id 不入账）；拿不到就不动作，
	// 避免留下「已登记未上报」的半程态。
	experts, lerr := p.marketExpertList(ctx, a, "agent", "", 1, 20)
	if lerr != nil || len(experts) == 0 {
		return "专家市场不可用，本轮跳过", nil
	}
	e := experts[0]
	if err := p.reportMPEvents(ctx, a, miniExpertUseEvent(e)); err != nil {
		return "上报失败：" + err.Error(), nil
	}
	// 异步计分：两轮回读（与领奖轮询同预算）
	for i := 0; i < 2; i++ {
		sleepCtx(ctx, 3*time.Second)
		if ts, rerr := p.listTasks(ctx, a); rerr == nil {
			if cur := findTask(ts, "Sequential_Tasks_2"); cur != nil {
				t = cur
				if isClaimable(*cur) || cur.AcceptStatus == "claimed" || cur.Status == "claimed" {
					break
				}
			}
		}
	}
	if t.AcceptStatus == "claimed" || t.Status == "claimed" {
		return "已入账", nil
	}
	if isClaimable(*t) {
		credit, _, cerr := p.claimReward(ctx, a, "Sequential_Tasks_2", true)
		if cerr == nil && credit > 0 {
			return fmt.Sprintf("已召唤「%s」并领取 +%d", e.DisplayNameZH, credit), nil
		}
		return "已上报，领奖未成功（下次重试）", nil
	}
	return fmt.Sprintf("已上报「%s」的专家对话事件（异步计分中）", e.DisplayNameZH), nil
}

// actionDescFor 任务的动作说明（面板「自动动作」列）。专家类任务不在动作表里，
// 但用户需要知道它会被跑，所以单独补一段文案。
func actionDescFor(code string) string {
	if act, ok := actionFor(code); ok {
		return act.desc
	}
	if kw, ok := namedExpertTargets[code]; ok {
		return "召唤「" + kw + "」类真实专家，一次极短对话（约 30 token）"
	}
	switch code {
	case "expert_5":
		return "召唤 5 位真实专家各一次极短对话（每次约 30 token，与具名任务共用）"
	case "Expert_team_use_3":
		return "召唤 3 个真实专家团各一次极短对话（每次约 30 token）"
	}
	return ""
}

// ── 编排 ─────────────────────────────────────────────────

// runSkillFresh skill_1「尝鲜热门技能」：一次极短真实对话 + 技能加载事件。
// 判据要真实 requestId（自造不计数），但对话内容可以只有一个字符。
func runSkillFresh(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	conv, reqID, err := p.desktopChatMinimal(ctx, a, "")
	if err != nil {
		return "", err
	}
	events := desktopChatSequence(conv, reqID, "msg-"+tail8(reqID), "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 「模型发起了工具调用」= 技能加载语义
		}
	}
	events = append(events, desktopSkillInfoEvent(conv, reqID))
	if err := p.reportDesktop(ctx, a, events...); err != nil {
		return "", err
	}
	return "已上报一次极短对话 + 技能加载事件", nil
}

// runSequentialChat1 Sequential_Tasks_1「完成 1 次对话」（小程序口径）。
// 判据是 mp 指纹的 chat_request_send，不需要真实对话，零 token。
func runSequentialChat1(ctx context.Context, p *Provider, a *model.Account) (string, error) {
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return "", err
	}
	need := needOf(tasks, "Sequential_Tasks_1", 1)
	for i := int64(0); i < need; i++ {
		if ctx.Err() != nil {
			break
		}
		conv := fmt.Sprintf("ccpa-mp-%d-%d", time.Now().UnixMilli(), i)
		if err := p.reportMPChatEvent(ctx, a, conv); err != nil {
			return fmt.Sprintf("第 %d 条上报失败：%v", i+1, err), nil
		}
		if i < need-1 {
			sleepCtx(ctx, 5*time.Second)
		}
	}
	return fmt.Sprintf("已上报 %d 条小程序对话事件", need), nil
}

// isClaimable 奖励是否可领：只要 target>0 且进度达标，就别去信 accept_status
// （上游回读时会短暂把它写成 completed 再回滚，据此领奖会误领）。
func isClaimable(t growthTask) bool {
	if t.AcceptStatus == "claimed" || t.Status == "claimed" || t.Locked {
		return false
	}
	return t.Target > 0 && t.Current >= t.Target
}

// RunTasks 自动完成任务中心：列表 → 接受 → 补进度 → 等结算 → 领奖。
// 返回 (本轮获得的积分, 摘要)。
func (p *Provider) RunTasks(ctx context.Context, a *model.Account) (int64, string, error) {
	// 成长体系只有国内个人版有：国际版 / 企业版请求过去只会拿到 403，不必白跑
	if isGlobal(a) {
		return 0, "国际版没有成长任务体系（上游仅对国内个人版开放）", nil
	}
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return 0, "", err
	}
	var parts []string

	// 阶段一：接受未接受的任务。两种口径必须分开提交 —— 混在一起会让
	// 小程序任务返回 task not found，整批失败。
	var codes, mpCodes []string
	for _, t := range tasks {
		if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" || t.AcceptStatus == "claimed" {
			continue
		}
		if isMPTask(t.TaskCode) {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			codes = append(codes, t.TaskCode)
		}
	}
	accepted := 0
	for _, g := range []struct {
		codes []string
		mp    bool
	}{{codes, false}, {mpCodes, true}} {
		if len(g.codes) == 0 {
			continue
		}
		if _, err := p.growthJSON(ctx, a, http.MethodPost, "/v2/activity/growth/tasks/accept",
			map[string]any{"task_codes": g.codes}, g.mp); err == nil {
			accepted += len(g.codes)
		}
	}
	if accepted > 0 {
		parts = append(parts, fmt.Sprintf("接受 %d 个任务", accepted))
	}

	// 阶段二：把「有动作但没达标」的任务推进度。已达标的不重复跑，
	// 避免每天白打一遍事件。
	//
	// 专家类任务先单独跑：一次「召唤 + 真实对话 + 使用事件」同时给 expert_5
	// 计一次、并点亮对应的 expert_use_xxx。走单任务循环会把同一位专家重复对话，
	// 白烧 token，所以这里合并处理，处理过的任务码在下面跳过。
	acted := 0
	handled := map[string]bool{}
	if expertParts, h := p.runExpertGroup(ctx, a, tasks); len(expertParts) > 0 || len(h) > 0 {
		parts = append(parts, expertParts...)
		handled = h
		if len(expertParts) > 0 {
			acted++
		}
	}

	for _, t := range tasks {
		if t.AcceptStatus == "claimed" || t.Status == "claimed" || t.Locked || isClaimable(t) {
			continue
		}
		if handled[t.TaskCode] {
			continue
		}
		act, ok := actionFor(t.TaskCode)
		if !ok {
			continue
		}
		msg, aerr := act.run(ctx, p, a)
		switch {
		case aerr != nil:
			parts = append(parts, fmt.Sprintf("%s 失败：%v", taskLabel(t), aerr))
		case strings.TrimSpace(msg) != "":
			parts = append(parts, fmt.Sprintf("%s：%s", taskLabel(t), msg))
			acted++
		}
		sleepCtx(ctx, reportGap)
	}

	// 阶段三：上游计分是异步的（事件上报后进度要数秒才刷新），
	// 一次性回读会误判「没达标」从而漏领，这里做有界轮询。
	if acted > 0 {
		for i := 0; i < 3; i++ {
			sleepCtx(ctx, 2500*time.Millisecond)
			if ts, err := p.listTasks(ctx, a); err == nil {
				tasks = ts
			}
			claimable := false
			for _, t := range tasks {
				if isClaimable(t) {
					claimable = true
					break
				}
			}
			if claimable {
				break
			}
		}
	}

	// 阶段四：领奖。claim 幂等（重复领返回 already_claimed），宁可多试不漏领。
	var earned int64
	for _, t := range tasks {
		if !isClaimable(t) {
			continue
		}
		credit, _, cerr := p.claimReward(ctx, a, t.TaskCode, isMPTask(t.TaskCode))
		if cerr != nil || credit <= 0 {
			continue
		}
		earned += credit
		parts = append(parts, fmt.Sprintf("%s +%d", taskLabel(t), credit))
	}

	if len(parts) == 0 {
		parts = append(parts, "暂无可自动完成的任务")
	}
	return earned, strings.Join(parts, "；"), nil
}

// GrowthTasks 列出成长任务及进度（只读，不发任何行为事件）。
// 面板用它回答「现在到底卡在哪个任务上」——没有这份快照，
// 「一键完成」就是个黑盒。
func (p *Provider) GrowthTasks(ctx context.Context, a *model.Account) ([]model.GrowthTask, error) {
	if isGlobal(a) {
		return nil, fmt.Errorf("国际版没有成长任务体系")
	}
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]model.GrowthTask, 0, len(tasks))
	for _, t := range tasks {
		v := model.GrowthTask{
			Code:      t.TaskCode,
			Title:     taskLabel(t),
			Target:    t.Target,
			Current:   t.Current,
			Claimable: isClaimable(t),
			Claimed:   t.AcceptStatus == "claimed" || t.Status == "claimed",
			Locked:    t.Locked,
		}
		if d := actionDescFor(t.TaskCode); d != "" {
			v.ActionDesc = d
		}
		out = append(out, v)
	}
	return out, nil
}

// taskLabel 摘要里用标题，标题缺失退回任务码。
func taskLabel(t growthTask) string {
	if strings.TrimSpace(t.Title) != "" {
		return t.Title
	}
	return t.TaskCode
}

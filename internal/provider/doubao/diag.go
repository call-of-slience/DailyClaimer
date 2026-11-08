package doubao

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"chinacpa/internal/netdiag"
)

// Diag 豆包链路自检：用与 validate 完全相同的请求（POST /samantha/aispace/homepage），
// 分「直连 / 环境变量代理 / Windows 系统代理」三条路各投一遍。
//
// 这条请求是本程序判断「网络通不通」的唯一标准，因为它是导入凭证时第一个要打的
// 端点。三条路的结论能直接区分：本机出网被拦、代理规则把豆包甩去境外、系统代理
// 开关残留、上游改版（404）——这四种在用户看来都长成「连不上」。
func (p *Provider) Diag(ctx context.Context) netdiag.Report {
	ids := deviceIDs{DeviceID: genDeviceID(), WebID: genWebID()}
	q := commonQuery(ids, "verify_diag_probe", "CN")
	req := netdiag.Request{
		Host:        "www.doubao.com",
		Method:      http.MethodPost,
		Path:        "/samantha/aispace/homepage?" + q.Encode(),
		ContentType: "application/json",
		Body:        []byte("{}"),
		Headers: map[string]string{
			"Origin":  dbOrigin,
			"Referer": dbOrigin + "/chat/",
		},
	}
	routes := netdiag.Check(ctx, req)

	var extra []string
	var saw200, saw404, saw302, sawLoginInvalid, saw5xx bool
	for _, r := range routes {
		for _, s := range r.Steps {
			if s.Name != "HTTPS 请求" {
				continue
			}
			switch {
			case strings.Contains(s.Detail, "HTTP 200"):
				saw200 = true
				if strings.Contains(s.Detail, "710012001") || strings.Contains(s.Detail, "login invalid") {
					sawLoginInvalid = true
				}
			case strings.Contains(s.Detail, "HTTP 404"):
				saw404 = true
			case strings.Contains(s.Detail, "HTTP 302"), strings.Contains(s.Detail, "HTTP 301"):
				saw302 = true
			case strings.Contains(s.Detail, "HTTP 5"):
				saw5xx = true
			}
		}
	}

	switch {
	case sawLoginInvalid:
		extra = append(extra, "✓ 上游返回 710012001「登录已过期」= 接口活着、链路通，只是这次探测没带 Cookie。")
		extra = append(extra, "  → 网络这一层可以排除了，导入失败就是 Cookie 本身的问题（缺 sessionid / 复制不全 / 已被顶号）。")
	case saw200:
		extra = append(extra, "✓ 接口返回 200，链路通。若导入仍失败，看返回体里的 code 字段。")
	case saw404:
		extra = append(extra, "△ 接口返回 404：网络能到，但路径不存在——要么上游改版（需更新程序），要么被代理改写到了别的节点。")
		extra = append(extra, "  → 对比三条路：只有某条路 404，就是那条路的问题；三条全 404 则基本确认上游改版。")
	case saw302:
		extra = append(extra, "△ 上游 302 到登录页：凭证/ Cookie 无效，不是网络问题。")
	case saw5xx:
		extra = append(extra, "△ 上游 5xx：字节侧故障或风控限流，隔几分钟再试。")
	}
	if p.checkinBrowser {
		extra = append(extra, "提示：签到走无头浏览器通道。浏览器读的是「Windows 系统代理」，")
		extra = append(extra, "     系统代理开关残留（代理软件关了但 ProxyEnable 还是 1）会让签到报连不上，")
		extra = append(extra, "     本程序已用 --no-proxy-server 让该浏览器直连豆包。")
	}
	extra = append(extra, fmt.Sprintf("提示：程序当前用哪条路由由 config.json → doubao.proxy 决定（空=跟随环境变量，direct=强制直连，也可填 http://127.0.0.1:端口）。"))

	return netdiag.Report{
		Host:    req.Host,
		Path:    req.Path,
		Method:  req.Method,
		Routes:  routes,
		Summary: netdiag.Summary(req.Host, routes),
		Extra:   extra,
	}
}

package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

// Companion 旁路服务的代理配置。凭据只从服务端环境变量读取，绝不下发到浏览器。
const (
	envCompanionBaseURL   = "COMPANION_BASE_URL"
	envCompanionAdminUser = "COMPANION_ADMIN_USER"
	envCompanionAdminPass = "COMPANION_ADMIN_PASSWORD"
	envCompanionTimeout   = "COMPANION_HTTP_TIMEOUT"

	companionDefaultTimeout = 20 * time.Second
	// companionMaxResponseBytes 限制上游响应体，避免异常大响应打爆面板内存。
	companionMaxResponseBytes = 8 << 20
	// companionMaxRequestBytes 限制透传的请求体大小。
	companionMaxRequestBytes = 1 << 20
)

// companionAccountIDPattern 限制账号规则路径参数，既贴合 Sub2API 的数字账号 ID，
// 也避免把前端输入拼进上游 URL 造成路径穿越。
var companionAccountIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// CompanionHandler 把 Companion 旁路服务（经营对账 / 上游账单归集）的 /ops/api/* 接口
// 经管理员鉴权后代理给管理后台页面。
//
// 安全设计：
//   - 浏览器侧鉴权完全复用管理后台的 admin 中间件链（在路由注册处绑定）；
//   - Companion 自身的 HTTP Basic 凭据只存在于服务端进程；
//   - 上游路径走硬编码白名单，绝不接受前端传入任意路径，避免退化成开放代理（SSRF）。
type CompanionHandler struct {
	baseURL string
	user    string
	pass    string
	client  *http.Client
}

// NewCompanionHandler 从环境变量构造代理处理器。未配置 COMPANION_BASE_URL 时
// 所有接口返回 503 / COMPANION_NOT_CONFIGURED，管理后台页面据此展示引导态。
func NewCompanionHandler() *CompanionHandler {
	timeout := companionDefaultTimeout
	if raw := strings.TrimSpace(os.Getenv(envCompanionTimeout)); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	return &CompanionHandler{
		baseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envCompanionBaseURL)), "/"),
		user:    strings.TrimSpace(os.Getenv(envCompanionAdminUser)),
		pass:    os.Getenv(envCompanionAdminPass),
		client:  &http.Client{Timeout: timeout},
	}
}

// Enabled 报告 Companion 代理是否已配置上游地址。
func (h *CompanionHandler) Enabled() bool {
	return h != nil && h.baseURL != ""
}

// companionUpstream 是一次上游调用的结果。
type companionUpstream struct {
	status int
	body   []byte
}

// do 调用白名单内的上游路径，透传查询串，并附带服务端持有的 Basic 凭据。
func (h *CompanionHandler) do(c *gin.Context, method, upstreamPath string, body []byte) (*companionUpstream, error) {
	target, err := url.Parse(h.baseURL + upstreamPath)
	if err != nil {
		return nil, err
	}
	if raw := c.Request.URL.RawQuery; raw != "" {
		target.RawQuery = raw
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.user != "" || h.pass != "" {
		req.SetBasicAuth(h.user, h.pass)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, companionMaxResponseBytes))
	if err != nil {
		return nil, err
	}
	return &companionUpstream{status: resp.StatusCode, body: payload}, nil
}

// upstreamMessage 尽力从上游错误体里提取可读信息。
func upstreamMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil {
		if envelope.Error != "" {
			return envelope.Error
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}
	text := string(trimmed)
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// relay 以管理后台统一信封返回上游结果。
//
// 上游的鉴权/配置类错误刻意映射成 502 而不是原样透传 401/403：面板前端把 401 视为
// 会话失效并跳登录页，若 Companion 凭据配错就会把管理员踢出去，难以排查。
func (h *CompanionHandler) relay(c *gin.Context, method, upstreamPath string, body []byte) {
	if !h.Enabled() {
		response.Error(c, http.StatusServiceUnavailable, "COMPANION_NOT_CONFIGURED")
		return
	}

	result, err := h.do(c, method, upstreamPath, body)
	if err != nil {
		response.Error(c, http.StatusBadGateway, "COMPANION_UNREACHABLE: "+err.Error())
		return
	}

	if result.status < http.StatusOK || result.status >= http.StatusMultipleChoices {
		detail := upstreamMessage(result.body)
		switch result.status {
		case http.StatusBadRequest:
			response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: "+detail)
		case http.StatusUnauthorized, http.StatusForbidden:
			response.Error(c, http.StatusBadGateway,
				"COMPANION_AUTH_FAILED: 请检查 COMPANION_ADMIN_USER / COMPANION_ADMIN_PASSWORD 是否与 Companion 一致")
		default:
			response.Error(c, http.StatusBadGateway,
				"COMPANION_UPSTREAM_"+strings.TrimSpace(http.StatusText(result.status))+": "+detail)
		}
		return
	}

	if len(bytes.TrimSpace(result.body)) == 0 {
		response.Success(c, gin.H{"ok": true})
		return
	}

	var payload json.RawMessage
	if err := json.Unmarshal(result.body, &payload); err != nil {
		response.Error(c, http.StatusBadGateway, "COMPANION_INVALID_RESPONSE: 上游返回了非 JSON 内容")
		return
	}
	response.Success(c, payload)
}

// requestBody 读取并限制透传的请求体。
func requestBody(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(c.Request.Body, companionMaxRequestBytes))
}

// Status 返回代理配置状态与 Companion 健康检查结果，供页面展示引导/告警态。
func (h *CompanionHandler) Status(c *gin.Context) {
	if !h.Enabled() {
		response.Success(c, gin.H{"enabled": false, "healthy": false})
		return
	}
	result, err := h.do(c, http.MethodGet, "/health", nil)
	if err != nil {
		response.Success(c, gin.H{
			"enabled": true,
			"healthy": false,
			"detail":  err.Error(),
		})
		return
	}
	healthy := result.status >= http.StatusOK && result.status < http.StatusMultipleChoices
	payload := gin.H{"enabled": true, "healthy": healthy, "status": result.status}
	if !healthy {
		payload["detail"] = upstreamMessage(result.body)
	}
	response.Success(c, payload)
}

// Summary 代理经营看板汇总。
func (h *CompanionHandler) Summary(c *gin.Context) {
	h.relay(c, http.MethodGet, "/ops/api/summary", nil)
}

// Timeseries 代理经营看板趋势分桶。
func (h *CompanionHandler) Timeseries(c *gin.Context) {
	h.relay(c, http.MethodGet, "/ops/api/timeseries", nil)
}

// Requests 代理经营看板明细分页。
func (h *CompanionHandler) Requests(c *gin.Context) {
	h.relay(c, http.MethodGet, "/ops/api/requests", nil)
}

// AccountRules 代理上游账号规则视图。
func (h *CompanionHandler) AccountRules(c *gin.Context) {
	h.relay(c, http.MethodGet, "/ops/api/account-rules", nil)
}

// accountRulePath 校验并拼接账号规则的上游路径。
func accountRulePath(c *gin.Context) (string, bool) {
	accountID := strings.TrimSpace(c.Param("account_id"))
	if !companionAccountIDPattern.MatchString(accountID) {
		response.BadRequest(c, "account_id 必须是数字账号 ID")
		return "", false
	}
	return "/ops/api/account-rules/" + accountID, true
}

// UpsertAccountRule 代理账号规则的新增/更新。
func (h *CompanionHandler) UpsertAccountRule(c *gin.Context) {
	path, ok := accountRulePath(c)
	if !ok {
		return
	}
	body, err := requestBody(c)
	if err != nil {
		response.BadRequest(c, "读取请求体失败: "+err.Error())
		return
	}
	h.relay(c, http.MethodPut, path, body)
}

// DeleteAccountRule 代理账号规则的删除。
func (h *CompanionHandler) DeleteAccountRule(c *gin.Context) {
	path, ok := accountRulePath(c)
	if !ok {
		return
	}
	h.relay(c, http.MethodDelete, path, nil)
}

// Collect 代理「立即同步」。
func (h *CompanionHandler) Collect(c *gin.Context) {
	body, err := requestBody(c)
	if err != nil {
		response.BadRequest(c, "读取请求体失败: "+err.Error())
		return
	}
	h.relay(c, http.MethodPost, "/ops/api/collect", body)
}

// A6BackfillStatus 代理 A6 历史回填状态查询。
func (h *CompanionHandler) A6BackfillStatus(c *gin.Context) {
	h.relay(c, http.MethodGet, "/ops/api/a6/backfill", nil)
}

// StartA6Backfill 代理 A6 历史回填启动。
func (h *CompanionHandler) StartA6Backfill(c *gin.Context) {
	body, err := requestBody(c)
	if err != nil {
		response.BadRequest(c, "读取请求体失败: "+err.Error())
		return
	}
	h.relay(c, http.MethodPost, "/ops/api/a6/backfill", body)
}

// ImportUpstream 代理上游逐笔账单导入。
func (h *CompanionHandler) ImportUpstream(c *gin.Context) {
	body, err := requestBody(c)
	if err != nil {
		response.BadRequest(c, "读取请求体失败: "+err.Error())
		return
	}
	h.relay(c, http.MethodPost, "/ops/api/upstream/import", body)
}

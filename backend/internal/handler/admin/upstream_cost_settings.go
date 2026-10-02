package admin

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UpstreamCostSettingsHandler 暴露「上游 A6 凭据」的读写接口。
//
// 为什么单独开一个 handler 而不是塞回原来那个：这段配置**只有一个消费者**——
// 后台按请求 ID 反查上游真实成本的那个取数任务。它有自己的生命周期与失败模式
// （凭据没配 = 成本永远取不到），和普通系统设置混在一起会让「成本为什么是空的」
// 这类问题变得难以定位。放在这里，接口路径本身就说明了它服务于什么。
//
// 只做转发与脱敏，所有校验（URL 合法性、令牌加密、覆盖优先级）都在
// service.ReconciliationA6SettingsService 里，不在这里重复实现一遍——
// 两处校验迟早会分叉，然后以「面板保存成功但实际没生效」的形式暴露出来。
type UpstreamCostSettingsHandler struct {
	settingsSvc *service.ReconciliationA6SettingsService
}

// NewUpstreamCostSettingsHandler 创建 A6 凭据设置 handler。
func NewUpstreamCostSettingsHandler(settingsSvc *service.ReconciliationA6SettingsService) *UpstreamCostSettingsHandler {
	return &UpstreamCostSettingsHandler{settingsSvc: settingsSvc}
}

// upstreamCostSettingsDTO 是 GET/PUT settings 的 data。
//
// 令牌只以「是否已配置 + 脱敏提示」两种形态出现，**任何情况下都不返回明文**：
// 这个结构体会被 JSON 序列化后发到浏览器，多一个字段就等于把令牌泄到前端。
type upstreamCostSettingsDTO struct {
	A6BaseURL         string   `json:"a6_base_url"`
	A6UserID          string   `json:"a6_user_id"`
	A6TokenConfigured bool     `json:"a6_token_configured"`
	A6TokenMask       string   `json:"a6_token_mask"`
	OverrideKeys      []string `json:"override_keys"`
}

func toUpstreamCostSettingsDTO(view service.ReconciliationA6SettingsView) upstreamCostSettingsDTO {
	// 前端直接遍历 override_keys，null 会让它崩掉。
	keys := view.OverrideKeys
	if keys == nil {
		keys = []string{}
	}
	return upstreamCostSettingsDTO{
		A6BaseURL:         view.A6BaseURL,
		A6UserID:          view.A6UserID,
		A6TokenConfigured: view.A6TokenConfigured,
		A6TokenMask:       view.A6TokenMask,
		OverrideKeys:      keys,
	}
}

// upstreamCostSettingsInputDTO 是 PUT settings 的入参。
//
// 全部字段可选（nil = 不改动），与 service 层入参的指针语义一一对应：
// 「没传这个字段」和「传了空串」必须能区分开，否则前端无法表达
// 「我只想改地址、令牌保持不动」——那会导致每次保存都把令牌清空。
type upstreamCostSettingsInputDTO struct {
	A6BaseURL          *string `json:"a6_base_url"`
	A6UserID           *string `json:"a6_user_id"`
	A6AccessToken      *string `json:"a6_access_token"`
	ClearA6AccessToken bool    `json:"clear_a6_access_token"`
}

// Settings 返回 A6 上游配置的生效状态。
//
// GET /admin/usage/upstream-cost/settings
func (h *UpstreamCostSettingsHandler) Settings(c *gin.Context) {
	if h == nil || h.settingsSvc == nil {
		response.InternalError(c, "UPSTREAM_COST_INTERNAL: A6 设置服务未装配")
		return
	}
	response.Success(c, toUpstreamCostSettingsDTO(h.settingsSvc.Effective(c.Request.Context()).View()))
}

// UpdateSettings 保存 A6 上游配置覆盖值。
//
// PUT /admin/usage/upstream-cost/settings
func (h *UpstreamCostSettingsHandler) UpdateSettings(c *gin.Context) {
	if h == nil || h.settingsSvc == nil {
		response.InternalError(c, "UPSTREAM_COST_INTERNAL: A6 设置服务未装配")
		return
	}

	var input upstreamCostSettingsInputDTO
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "UPSTREAM_COST_BAD_REQUEST: 请求体不是合法的 JSON: "+err.Error())
		return
	}

	settings, err := h.settingsSvc.Update(c.Request.Context(), service.ReconciliationA6SettingsInput{
		BaseURL:          input.A6BaseURL,
		UserID:           input.A6UserID,
		AccessToken:      input.A6AccessToken,
		ClearAccessToken: input.ClearA6AccessToken,
	})
	if err != nil {
		writeUpstreamCostSettingsError(c, err)
		return
	}

	response.Success(c, toUpstreamCostSettingsDTO(settings.View()))
}

// writeUpstreamCostSettingsError 把设置服务的哨兵错误翻译成 HTTP 状态码。
//
// 逐条映射而不是一律 500：这些错误的处置方式完全不同——URL 填错要人改，
// 加密器缺席要找运维。全都回 500 的话，面板只能显示「服务器错误」，
// 而真正该看到那句话的人（正在填表单的管理员）拿不到任何可行动的提示。
func writeUpstreamCostSettingsError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrReconciliationInvalidBaseURL):
		response.BadRequest(c, "UPSTREAM_COST_INVALID_BASE_URL: A6 站点地址必须是合法的 http/https 地址")
	case errors.Is(err, service.ErrReconciliationInvalidFxRate):
		response.BadRequest(c, "UPSTREAM_COST_INVALID_FX_RATE: 汇率取值越界")
	case errors.Is(err, service.ErrReconciliationSecretEncryptorUnavailable):
		// 拒绝落明文是**有意**的：这是一张普通键值表，明文令牌会被任何一次
		// 库备份带走。这里必须把原因说清楚，否则运维会以为是随机故障。
		response.InternalError(c, "UPSTREAM_COST_ENCRYPTOR_UNAVAILABLE: 加密器未配置，拒绝以明文保存令牌")
	case errors.Is(err, service.ErrReconciliationSettingsStoreUnavailable):
		response.InternalError(c, "UPSTREAM_COST_STORE_UNAVAILABLE: 配置存储不可用")
	default:
		response.InternalError(c, "UPSTREAM_COST_INTERNAL: 保存 A6 配置失败: "+err.Error())
	}
}

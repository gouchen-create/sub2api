package handler

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 公开作品墙的返回上限与安全头。
const (
	// publicIntelligenceCheckMaxCards 单次最多返回多少张卡片，避免超大站点一次拉全量。
	publicIntelligenceCheckMaxCards = 200
	publicIntelligenceCheckCSP      = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; script-src 'none'"
)

// IntelligenceCheckPublicHandler 是智力检测的**用户侧只读脱敏**接口。
//
// 与 admin 侧的关键差别：这里从不返回 account_id、账号名、上游标识与模型名。
// 卡片只带一个从 1 开始的展示序号，作品通过 run id 单独取回，因此普通用户
// 看得到「有几个账号在跑、跑得怎么样」，看不出这些账号是谁、接的哪家上游。
type IntelligenceCheckPublicHandler struct {
	checkService *service.IntelligenceCheckService
}

// NewIntelligenceCheckPublicHandler 构造用户侧只读 handler。
func NewIntelligenceCheckPublicHandler(checkService *service.IntelligenceCheckService) *IntelligenceCheckPublicHandler {
	return &IntelligenceCheckPublicHandler{checkService: checkService}
}

// publicIntelligenceCheckCard 是脱敏后的作品墙卡片。
type publicIntelligenceCheckCard struct {
	Index       int    `json:"index"`
	Status      string `json:"status"`
	Verdict     string `json:"verdict"`
	LatencyMs   int64  `json:"latency_ms"`
	ErrorCode   string `json:"error_code,omitempty"`
	HasArtifact bool   `json:"has_artifact"`
	ArtifactURL string `json:"artifact_url,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// ListPublicRuns 返回脱敏后的作品墙卡片：每个参与过跑测的账号一张，按序号升序。
func (h *IntelligenceCheckPublicHandler) ListPublicRuns(c *gin.Context) {
	if h == nil || h.checkService == nil {
		response.Success(c, gin.H{"items": []publicIntelligenceCheckCard{}, "total": 0})
		return
	}

	runs, err := h.checkService.ListLatestPerAccount(c.Request.Context(), publicIntelligenceCheckMaxCards)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}

	items := make([]publicIntelligenceCheckCard, 0, len(runs))
	for _, run := range runs {
		if run == nil {
			continue
		}
		// 序号按「实际渲染出来的卡片」递增，中间跳过 nil 也不会出现断号。
		card := publicIntelligenceCheckCard{
			Index:       len(items) + 1,
			Status:      run.Status,
			Verdict:     run.Verdict,
			LatencyMs:   run.LatencyMs,
			ErrorCode:   run.ErrorCode,
			HasArtifact: run.HasHTML,
			CreatedAt:   run.CreatedAt.Format(time.RFC3339),
		}
		if run.HasHTML {
			card.ArtifactURL = "/api/v1/intelligence-check/runs/" + strconv.FormatInt(run.ID, 10) + "/artifact"
		}
		items = append(items, card)
	}

	response.Success(c, gin.H{"items": items, "total": len(items)})
}

// GetPublicArtifact 返回作品原文，供前端以 srcdoc 方式在沙箱 iframe 中预览。
func (h *IntelligenceCheckPublicHandler) GetPublicArtifact(c *gin.Context) {
	if h == nil || h.checkService == nil {
		response.NotFound(c, "artifact not found")
		return
	}

	runID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || runID <= 0 {
		response.BadRequest(c, "invalid run id")
		return
	}

	artifact, err := h.checkService.GetArtifact(c.Request.Context(), runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "artifact not found")
			return
		}
		response.InternalError(c, err.Error())
		return
	}

	// 与 admin 侧同一条安全策略：即便有人直接打开这个 URL，脚本也不会执行。
	// 前端预览走 sandbox iframe + srcdoc，动画在那个隔离环境里跑。
	c.Header("Content-Security-Policy", publicIntelligenceCheckCSP)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(artifact.HTMLText))
}

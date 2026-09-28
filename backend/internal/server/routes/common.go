package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// RegisterCommonRoutes registers public common routes.
func RegisterCommonRoutes(r *gin.Engine, h *handler.Handlers) {
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	if h != nil && h.ProviderPricing != nil {
		r.GET("/api/provider/pricing", h.ProviderPricing.List)
	}

	// Claude Code telemetry logs are intentionally ignored.
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint, used by the frontend after setup restarts.
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}

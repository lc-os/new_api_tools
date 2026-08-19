package handler

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterQuotaRefreshRoutes registers /api/quota-refresh endpoints:
// config management + manual trigger + run history.
func RegisterQuotaRefreshRoutes(r *gin.RouterGroup) {
	rg := r.Group("/quota-refresh")
	{
		rg.GET("/config", GetQuotaRefreshConfig)
		rg.PUT("/config", UpdateQuotaRefreshConfig)
		rg.POST("/run", RunQuotaRefresh)
		rg.GET("/runs", GetQuotaRefreshRuns)
		rg.GET("/assignments", GetQuotaRefreshAssignments)
		rg.PUT("/assignments", UpdateQuotaRefreshAssignments)
		rg.POST("/assignments/generate", GenerateQuotaRefreshAssignments)
		rg.POST("/assignments/recommend", RecommendQuotaRefreshAssignments)
		rg.DELETE("/assignments", ClearQuotaRefreshAssignments)
		rg.GET("/caps", GetQuotaRefreshCaps)
		rg.PUT("/caps", UpdateQuotaRefreshCaps)
	}
}

// GET /api/quota-refresh/caps — per-token caps inside the shared pool
func GetQuotaRefreshCaps(c *gin.Context) {
	svc := service.NewQuotaRefreshService()
	caps, err := svc.GetTokenCaps()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"items":    caps,
		"default":  service.DefaultCapYuan,
		"total":    len(caps),
		"unlocked": func() int { n := 0; for _, v := range caps { if v.Unlocked { n++ } }; return n }(),
	}})
}

// PUT /api/quota-refresh/caps — save per-token caps and apply unlock/lock immediately
func UpdateQuotaRefreshCaps(c *gin.Context) {
	var req struct {
		Items []service.TokenCap `json:"items"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误: " + err.Error()}})
		return
	}
	svc := service.NewQuotaRefreshService()
	touched, err := svc.UpdateTokenCaps(req.Items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"message": fmt.Sprintf("已保存 %d 个令牌的上限配置并立即生效", touched),
		"touched": touched,
	}})
}

// GET /api/quota-refresh/config
func GetQuotaRefreshConfig(c *gin.Context) {
	svc := service.NewQuotaRefreshService()
	cfg, err := svc.GetConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"enabled":       cfg.Enabled,
		"quota_amount":  cfg.QuotaAmount,
		"refresh_time":  cfg.RefreshTime,
		"mode":          cfg.Mode,
		"shared_budget": cfg.SharedBudget,
		"last_run_at":   cfg.LastRunAt,
		"last_run_info": cfg.LastRunInfo,
		"updated_at":    cfg.UpdatedAt,
		"wallet_balance": cfg.WalletBalance,
	}})
}

// PUT /api/quota-refresh/config
// Body: { enabled: bool, quota_amount: int, refresh_time: "HH:mm",
//         mode: "uniform"|"per-token"|"shared", shared_budget: float 元 }
func UpdateQuotaRefreshConfig(c *gin.Context) {
	var req struct {
		Enabled      bool    `json:"enabled"`
		QuotaAmount  int64   `json:"quota_amount"`
		RefreshTime  string  `json:"refresh_time"`
		Mode         string  `json:"mode"`
		SharedBudget float64 `json:"shared_budget"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误"}})
		return
	}

	svc := service.NewQuotaRefreshService()
	cfg, err := svc.UpdateConfig(req.Enabled, req.QuotaAmount, req.RefreshTime, req.Mode, req.SharedBudget)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"enabled":       cfg.Enabled,
		"quota_amount":  cfg.QuotaAmount,
		"refresh_time":  cfg.RefreshTime,
		"mode":          cfg.Mode,
		"shared_budget": cfg.SharedBudget,
		"last_run_at":   cfg.LastRunAt,
		"last_run_info": cfg.LastRunInfo,
		"updated_at":    cfg.UpdatedAt,
	}})
}

// POST /api/quota-refresh/run — manually trigger one refresh right now
func RunQuotaRefresh(c *gin.Context) {
	svc := service.NewQuotaRefreshService()
	result, err := svc.RunNow()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// GET /api/quota-refresh/runs?limit=20
func GetQuotaRefreshRuns(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	svc := service.NewQuotaRefreshService()
	runs, err := svc.GetRuns(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"items": runs}})
}

// GET /api/quota-refresh/assignments — current per-token allocation table
func GetQuotaRefreshAssignments(c *gin.Context) {
	svc := service.NewQuotaRefreshService()
	data, err := svc.GetAssignments()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/quota-refresh/assignments/generate
// Body: { budget: float64 元, days?: int, mode?: "share" | "average" }
func GenerateQuotaRefreshAssignments(c *gin.Context) {
	var req struct {
		Budget float64 `json:"budget"`
		Days   int     `json:"days"`
		Mode   string  `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误"}})
		return
	}
	svc := service.NewQuotaRefreshService()
	data, err := svc.GenerateAssignments(req.Budget, req.Days, req.Mode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/quota-refresh/assignments/recommend
// Body: { budget: float64 元, days?: int }  按用量占比推荐两档额度（大/小）
func RecommendQuotaRefreshAssignments(c *gin.Context) {
	var req struct {
		Budget float64 `json:"budget"`
		Days   int     `json:"days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误"}})
		return
	}
	svc := service.NewQuotaRefreshService()
	data, err := svc.RecommendAssignments(req.Budget, req.Days)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// PUT /api/quota-refresh/assignments — save a manually edited per-token table
// Body: { items: [{ token_id, quota }] }  quota in quota units (元*500000)
func UpdateQuotaRefreshAssignments(c *gin.Context) {
	var req struct {
		Items []struct {
			TokenID int64 `json:"token_id"`
			Quota   int64 `json:"quota"`
		} `json:"items"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误"}})
		return
	}
	items := make([]service.QuotaAssignment, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, service.QuotaAssignment{TokenID: it.TokenID, Quota: it.Quota})
	}
	svc := service.NewQuotaRefreshService()
	data, err := svc.UpdateAssignments(items)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// DELETE /api/quota-refresh/assignments — clear table, fall back to uniform quota
func ClearQuotaRefreshAssignments(c *gin.Context) {
	svc := service.NewQuotaRefreshService()
	if err := svc.ClearAssignments(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"message": "分配表已清空，恢复统一额度模式"}})
}

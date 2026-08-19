package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterPeakPricingRoutes registers /api/peak-pricing endpoints:
// DeepSeek peak/off-peak time-based pricing config + manual apply/capture.
func RegisterPeakPricingRoutes(r *gin.RouterGroup) {
	rg := r.Group("/peak-pricing")
	{
		rg.GET("/config", GetPeakPricingConfig)
		rg.PUT("/config", UpdatePeakPricingConfig)
		rg.POST("/apply", ApplyPeakPricing)
		rg.POST("/capture", CapturePeakPricingBaseline)
	}
}

// GET /api/peak-pricing/config
func GetPeakPricingConfig(c *gin.Context) {
	svc := service.NewPeakPricingService()
	cfg, err := svc.GetConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	baseline, _ := svc.GetBaseline()
	// 模型定价（元/百万tokens），直接读取自捕获的高峰价基准
	prices, _ := svc.GetModelPricing(cfg.PeakMultiplier)
	// 当前时刻属于哪个时段
	nowState := "offpeak"
	if svc.InPeakPeriod(cfg.PeakPeriods, time.Now()) {
		nowState = "peak"
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"enabled":        cfg.Enabled,
		"peak_periods":   cfg.PeakPeriods,
		"peak_multiplier": cfg.PeakMultiplier,
		"current_state":  cfg.CurrentState,
		"now_state":      nowState,
		"last_switch_at": cfg.LastSwitchAt,
		"updated_at":     cfg.UpdatedAt,
		"baseline":       baseline,
		"prices":         prices,
	}})
}

// PUT /api/peak-pricing/config
// Body: { enabled: bool, peak_periods: "09:00-12:00,14:00-18:00", peak_multiplier: 2 }
// 保存后立即生效：启用→按当前时段应用；停用→恢复默认空闲价；调整时段/倍数→重新应用。
func UpdatePeakPricingConfig(c *gin.Context) {
	var req struct {
		Enabled        bool    `json:"enabled"`
		PeakPeriods    string  `json:"peak_periods"`
		PeakMultiplier float64 `json:"peak_multiplier"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求体无效"}})
		return
	}
	svc := service.NewPeakPricingService()
	data, err := svc.UpdateConfig(req.Enabled, req.PeakPeriods, req.PeakMultiplier)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/peak-pricing/apply — 立即按当前时刻应用高峰/空闲价格
func ApplyPeakPricing(c *gin.Context) {
	svc := service.NewPeakPricingService()
	data, err := svc.ApplyNow()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/peak-pricing/capture — 重新捕获当前 new-api 定价为高峰价基准
func CapturePeakPricingBaseline(c *gin.Context) {
	svc := service.NewPeakPricingService()
	baseline, err := svc.CaptureBaseline()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": baseline})
}

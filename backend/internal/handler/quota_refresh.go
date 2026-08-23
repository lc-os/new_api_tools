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
		rg.GET("/off-days", ListQuotaRefreshOffDays)
		rg.PUT("/off-days", AddQuotaRefreshOffDay)
		rg.DELETE("/off-days", RemoveQuotaRefreshOffDay)
		rg.GET("/calendar", GetQuotaRefreshCalendar)
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
	// default 取配置值（未单独设置上限的令牌使用）
	cfg, err := svc.GetConfig()
	if err != nil {
		cfg = service.QuotaRefreshConfig{DefaultCapYuan: service.DefaultCapYuan}
	}
	defCap := cfg.DefaultCapYuan
	if defCap <= 0 {
		defCap = service.DefaultCapYuan
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"items":    caps,
		"default":  defCap,
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
		"enabled":          cfg.Enabled,
		"quota_amount":     cfg.QuotaAmount,
		"refresh_time":     cfg.RefreshTime,
		"mode":             cfg.Mode,
		"shared_budget":    cfg.SharedBudget,
		"schedule_mode":    cfg.ScheduleMode,
		"skip_weekends":    cfg.SkipWeekends,
		"skip_holidays":    cfg.SkipHolidays,
		"default_cap_yuan": cfg.DefaultCapYuan,
		"off_days":         cfg.OffDays,
		"last_run_at":      cfg.LastRunAt,
		"last_run_info":    cfg.LastRunInfo,
		"updated_at":       cfg.UpdatedAt,
		"wallet_balance":   cfg.WalletBalance,
		"next_run_at":      cfg.NextRunAt,
		"next_run_info":    cfg.NextRunInfo,
	}})
}

// PUT /api/quota-refresh/config
// Body: { enabled: bool, quota_amount: int, refresh_time: "HH:mm",
//         mode: "uniform"|"per-token"|"shared", shared_budget: float 元,
//         schedule_mode: "daily"|"workday"|"custom", default_cap_yuan: float 元 }
// 旧字段 skip_weekends/skip_holidays 仍接受（映射到 schedule_mode），保持兼容。
func UpdateQuotaRefreshConfig(c *gin.Context) {
	var req struct {
		Enabled         bool    `json:"enabled"`
		QuotaAmount     int64   `json:"quota_amount"`
		RefreshTime     string  `json:"refresh_time"`
		Mode            string  `json:"mode"`
		SharedBudget    float64 `json:"shared_budget"`
		ScheduleMode    string  `json:"schedule_mode"`
		SkipWeekends    *bool   `json:"skip_weekends"`
		SkipHolidays    *bool   `json:"skip_holidays"`
		DefaultCapYuan  float64 `json:"default_cap_yuan"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误"}})
		return
	}
	scheduleMode := req.ScheduleMode
	// 旧字段兼容：未传 schedule_mode 时由 skip_* 推导
	if scheduleMode == "" && (req.SkipWeekends != nil || req.SkipHolidays != nil) {
		skipW, skipH := false, false
		if req.SkipWeekends != nil {
			skipW = *req.SkipWeekends
		}
		if req.SkipHolidays != nil {
			skipH = *req.SkipHolidays
		}
		if skipW || skipH {
			scheduleMode = service.ScheduleWorkday
		} else {
			scheduleMode = service.ScheduleDaily
		}
	}

	svc := service.NewQuotaRefreshService()
	cfg, err := svc.UpdateConfig(req.Enabled, req.QuotaAmount, req.RefreshTime, req.Mode, req.SharedBudget,
		scheduleMode, req.DefaultCapYuan)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"enabled":          cfg.Enabled,
		"quota_amount":     cfg.QuotaAmount,
		"refresh_time":     cfg.RefreshTime,
		"mode":             cfg.Mode,
		"shared_budget":    cfg.SharedBudget,
		"schedule_mode":    cfg.ScheduleMode,
		"skip_weekends":    cfg.SkipWeekends,
		"skip_holidays":    cfg.SkipHolidays,
		"default_cap_yuan": cfg.DefaultCapYuan,
		"off_days":         cfg.OffDays,
		"last_run_at":      cfg.LastRunAt,
		"last_run_info":    cfg.LastRunInfo,
		"updated_at":       cfg.UpdatedAt,
		"next_run_at":      cfg.NextRunAt,
		"next_run_info":    cfg.NextRunInfo,
	}})
}

// GET /api/quota-refresh/off-days — 自定义休息日列表
func ListQuotaRefreshOffDays(c *gin.Context) {
	svc := service.NewQuotaRefreshService()
	items, err := svc.ListOffDays()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"items": items, "total": len(items)}})
}

// PUT /api/quota-refresh/off-days — 添加/更新自定义休息日
// Body: { date: "YYYY-MM-DD", note?: string }
func AddQuotaRefreshOffDay(c *gin.Context) {
	var req struct {
		Date string `json:"date"`
		Note string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "请求格式错误"}})
		return
	}
	svc := service.NewQuotaRefreshService()
	if err := svc.AddOffDay(req.Date, req.Note); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"message": "休息日已保存", "date": req.Date}})
}

// DELETE /api/quota-refresh/off-days?date=YYYY-MM-DD — 删除自定义休息日
func RemoveQuotaRefreshOffDay(c *gin.Context) {
	date := c.Query("date")
	if date == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "缺少 date 参数（YYYY-MM-DD）"}})
		return
	}
	svc := service.NewQuotaRefreshService()
	if err := svc.RemoveOffDay(date); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"message": "休息日已删除", "date": date}})
}

// GET /api/quota-refresh/calendar?days=30 — 未来 N 天日历标注（执行/跳过原因）
func GetQuotaRefreshCalendar(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	svc := service.NewQuotaRefreshService()
	items, err := svc.CalendarPreview(days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"items": items, "days": len(items)}})
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

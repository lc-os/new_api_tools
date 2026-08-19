package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterTokenUsageRoutes registers the token usage board endpoints.
// Restored from the production image: GET /api/token-usage/board
func RegisterTokenUsageRoutes(r *gin.RouterGroup) {
	rg := r.Group("/token-usage")
	{
		rg.GET("/board", GetTokenUsageBoard)
	}
}

// GetTokenUsageBoard handles GET /api/token-usage/board
// Query params: start, end (unix seconds), page (from 1), page_size (fixed 50),
// model (optional), token_id (optional, >0)
func GetTokenUsageBoard(c *gin.Context) {
	start, _ := strconv.ParseInt(c.Query("start"), 10, 64)
	end, _ := strconv.ParseInt(c.Query("end"), 10, 64)
	tokenID, _ := strconv.Atoi(c.Query("token_id"))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	model := c.Query("model")

	svc := service.NewTokenUsageService()
	data, err := svc.GetTokenUsageBoard(start, end, tokenID, model, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   gin.H{"message": err.Error()},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

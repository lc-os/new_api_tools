package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
)

// TokenUsageService handles the token usage board queries.
// Restored from the production image binary (route /api/token-usage/board),
// contract: summary / token_options / model_options / daily_top_users /
// daily_top_models / logs.
type TokenUsageService struct {
	db    *database.Manager
	logDB *database.Manager
}

// NewTokenUsageService creates a new TokenUsageService
func NewTokenUsageService() *TokenUsageService {
	return &TokenUsageService{db: database.Get(), logDB: database.GetLog()}
}

const tokenUsagePageSize = 50

// dayExpr returns the SQL expression that formats created_at as YYYY-MM-DD
func dayExpr(m *database.Manager) string {
	if m.IsPG {
		return `TO_CHAR(TO_TIMESTAMP(created_at), 'YYYY-MM-DD')`
	}
	return `DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d')`
}

// GetTokenUsageBoard returns the full usage board payload:
// summary cards, dropdown options, daily top users/models and paginated log detail.
func (s *TokenUsageService) GetTokenUsageBoard(start, end int64, tokenID int, model string, page, pageSize int) (map[string]interface{}, error) {
	cm := cache.Get()
	cacheKey := fmt.Sprintf("token_usage:%d:%d:%d:%s:%d:%d", start, end, tokenID, model, page, pageSize)
	var cached map[string]interface{}
	if found, _ := cm.GetJSON(cacheKey, &cached); found {
		return cached, nil
	}

	if pageSize <= 0 || pageSize > 50 {
		pageSize = tokenUsagePageSize
	}
	if page < 1 {
		page = 1
	}

	// type = 2 is consumption log in new-api (1=topup, 2=consume, 5=billing)
	conds := []string{"type = 2"}
	args := []interface{}{}

	// group is a reserved-ish word; quote per engine
	groupCol := "group"
	if s.logDB.IsPG {
		groupCol = `"group"`
	} else {
		groupCol = "`group`"
	}
	if start > 0 {
		conds = append(conds, "created_at >= ?")
		args = append(args, start)
	}
	if end > 0 {
		conds = append(conds, "created_at <= ?")
		args = append(args, end)
	}
	if tokenID > 0 {
		conds = append(conds, "token_id = ?")
		args = append(args, tokenID)
	}
	if model != "" {
		conds = append(conds, "model_name = ?")
		args = append(args, model)
	}
	where := "WHERE " + strings.Join(conds, " AND ")

	result := map[string]interface{}{}
	result["meta"] = map[string]interface{}{"generated_at": time.Now().Unix()}

	// ---------- summary ----------
	summaryQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT COUNT(*) AS request_count,
		       COUNT(DISTINCT token_id) AS token_count,
		       COUNT(DISTINCT model_name) AS model_count,
		       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
		       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS total_tokens
		FROM logs %s`, where))
	summaryRow, err := s.logDB.QueryOneWithTimeout(30*time.Second, summaryQuery, args...)
	if err != nil {
		return nil, err
	}
	if summaryRow != nil {
		result["summary"] = summaryRow
	}

	// ---------- dropdown options ----------
	tokenOptionsQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT token_id, MAX(token_name) AS user_name
		FROM logs %s
		GROUP BY token_id
		ORDER BY MAX(token_name)`, where))
	tokenOptionsRows, err := s.logDB.QueryWithTimeout(30*time.Second, tokenOptionsQuery, args...)
	if err != nil {
		return nil, err
	}
	tokenOptions := make([]map[string]interface{}, 0, len(tokenOptionsRows))
	for _, row := range tokenOptionsRows {
		tokenOptions = append(tokenOptions, map[string]interface{}{
			"token_id":  toInt64(row["token_id"]),
			"user_name": toString(row["user_name"]),
		})
	}
	result["token_options"] = tokenOptions

	modelOptionsQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT model_name, COUNT(*) AS request_count
		FROM logs %s
		GROUP BY model_name
		ORDER BY request_count DESC`, where))
	modelOptionsRows, err := s.logDB.QueryWithTimeout(30*time.Second, modelOptionsQuery, args...)
	if err != nil {
		return nil, err
	}
	modelOptions := make([]map[string]interface{}, 0, len(modelOptionsRows))
	for _, row := range modelOptionsRows {
		modelOptions = append(modelOptions, map[string]interface{}{
			"model_name":    toString(row["model_name"]),
			"request_count": toInt64(row["request_count"]),
		})
	}
	result["model_options"] = modelOptions

	// ---------- daily top users (per-day max request token) ----------
	day := dayExpr(s.logDB)
	dailyUsersQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT day, token_id, user_name, top_model_name, request_count, total_tokens
		FROM (
			SELECT day, token_id, user_name, model_name AS top_model_name,
			       request_count, total_tokens,
			       ROW_NUMBER() OVER (PARTITION BY day ORDER BY request_count DESC, token_id) AS rn
			FROM (
				SELECT %s AS day, token_id, MAX(token_name) AS user_name, model_name,
				       COUNT(*) AS request_count,
				       SUM(prompt_tokens + completion_tokens) AS total_tokens
				FROM logs %s
				GROUP BY day, token_id, model_name
			) grouped
		) ranked
		WHERE rn = 1
		ORDER BY day DESC`, day, where))
	dailyUsersRows, err := s.logDB.QueryWithTimeout(30*time.Second, dailyUsersQuery, args...)
	if err != nil {
		return nil, err
	}
	dailyUsers := make([]map[string]interface{}, 0, len(dailyUsersRows))
	for _, row := range dailyUsersRows {
		dailyUsers = append(dailyUsers, map[string]interface{}{
			"day":            toString(row["day"]),
			"token_id":       toInt64(row["token_id"]),
			"user_name":      toString(row["user_name"]),
			"top_model_name": toString(row["top_model_name"]),
			"request_count":  toInt64(row["request_count"]),
			"total_tokens":   toInt64(row["total_tokens"]),
		})
	}
	result["daily_top_users"] = dailyUsers

	// ---------- daily top models (per-day max request model) ----------
	dailyModelsQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT day, model_name, token_count, request_count, total_tokens
		FROM (
			SELECT day, model_name, token_count, request_count, total_tokens,
			       ROW_NUMBER() OVER (PARTITION BY day ORDER BY request_count DESC, model_name) AS rn
			FROM (
				SELECT %s AS day, model_name,
				       COUNT(DISTINCT token_id) AS token_count,
				       COUNT(*) AS request_count,
				       SUM(prompt_tokens + completion_tokens) AS total_tokens
				FROM logs %s
				GROUP BY day, model_name
			) grouped
		) ranked
		WHERE rn = 1
		ORDER BY day DESC`, day, where))
	dailyModelsRows, err := s.logDB.QueryWithTimeout(30*time.Second, dailyModelsQuery, args...)
	if err != nil {
		return nil, err
	}
	dailyModels := make([]map[string]interface{}, 0, len(dailyModelsRows))
	for _, row := range dailyModelsRows {
		dailyModels = append(dailyModels, map[string]interface{}{
			"day":           toString(row["day"]),
			"model_name":    toString(row["model_name"]),
			"token_count":   toInt64(row["token_count"]),
			"request_count": toInt64(row["request_count"]),
			"total_tokens":  toInt64(row["total_tokens"]),
		})
	}
	result["daily_top_models"] = dailyModels

	// ---------- paginated log detail ----------
	countQuery := s.logDB.RebindQuery(fmt.Sprintf("SELECT COUNT(*) AS total FROM logs %s", where))
	countRow, err := s.logDB.QueryOneWithTimeout(30*time.Second, countQuery, args...)
	if err != nil {
		return nil, err
	}
	total := int64(0)
	if countRow != nil {
		total = toInt64(countRow["total"])
	}

	offset := (page - 1) * pageSize
	logsQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT id, created_at, token_name AS user_name, token_id, model_name,
		       prompt_tokens, completion_tokens, channel_id, %s AS group_name, request_id
		FROM logs %s
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, groupCol, where))
	logArgs := append(append([]interface{}{}, args...), pageSize, offset)
	logsRows, err := s.logDB.QueryWithTimeout(30*time.Second, logsQuery, logArgs...)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]interface{}, 0, len(logsRows))
	for _, row := range logsRows {
		items = append(items, map[string]interface{}{
			"id":                toInt64(row["id"]),
			"created_at":        toInt64(row["created_at"]),
			"user_name":         toString(row["user_name"]),
			"token_id":          toInt64(row["token_id"]),
			"model_name":        toString(row["model_name"]),
			"prompt_tokens":     toInt64(row["prompt_tokens"]),
			"completion_tokens": toInt64(row["completion_tokens"]),
			"channel_id":        toInt64(row["channel_id"]),
			"group_name":        toString(row["group_name"]),
			"request_id":        toString(row["request_id"]),
		})
	}
	result["logs"] = map[string]interface{}{
		"total":     total,
		"page_size": pageSize,
		"items":     items,
	}

	cm.Set(cacheKey, result, 60*time.Second)
	return result, nil
}

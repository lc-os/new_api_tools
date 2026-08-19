package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
)

// QuotaRefreshService manages the daily token quota reset feature:
// at a configured time each day, every enabled token's remain_quota is
// reset to a fixed amount and unlimited_quota is turned off.
// Config + run history live in a local SQLite store (same pattern as abuse broadcast).
type QuotaRefreshService struct {
	cfg *config.Config
}

// QuotaRefreshConfig is the persisted single-row settings
type QuotaRefreshConfig struct {
	Enabled      bool    `json:"enabled"`
	QuotaAmount  int64   `json:"quota_amount"`  // per-token quota set on each refresh
	RefreshTime  string  `json:"refresh_time"`  // HH:mm, daily trigger time
	Mode         string  `json:"mode"`          // uniform | per-token | shared
	SharedBudget float64 `json:"shared_budget"` // 元，shared 模式全站每日总预算
	LastRunAt    int64   `json:"last_run_at"`
	LastRunInfo  string  `json:"last_run_info"`
	UpdatedAt    int64   `json:"updated_at"`
	WalletBalance int64  `json:"wallet_balance"` // 共享池当前剩余额度（启用令牌用户钱包合计）
}

// QuotaRefreshRun is one execution record
type QuotaRefreshRun struct {
	ID          int64  `json:"id"`
	RunAt       int64  `json:"run_at"`
	Triggered   int64  `json:"triggered"` // tokens actually reset
	QuotaAmount int64  `json:"quota_amount"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
}

// DefaultCapYuan is the per-token daily cap inside the shared pool (¥20)
const DefaultCapYuan = 20.0

// TokenCap is a per-token daily cap within the shared pool:
//   - capped tokens get remain_quota = cap (unlimited_quota=false), consumption
//     stops at the cap automatically (new-api 403 token quota exceeded)
//   - unlocked tokens are unlimited and draw from the shared pool wallet
//     (all stop together when the pool is exhausted)
type TokenCap struct {
	TokenID     int64   `json:"token_id"`
	TokenName   string  `json:"token_name"`
	CapYuan     float64 `json:"cap_yuan"` // 每人每日上限（元），默认 20；0 = 当天不分配额度
	Unlocked    bool    `json:"unlocked"` // 配置：已解除限制（走共享池）
	RemainQuota int64   `json:"remain_quota"` // 令牌当前剩余额度（unlimited 时无意义）
	Unlimited   bool    `json:"unlimited"` // 线上实际状态：无限额（走钱包/共享池）
	UsedQuota   int64   `json:"used_quota"` // 当天 0 点起该令牌已用额度（logs type=2）
	CacheHitRate float64 `json:"cache_hit_rate"` // 当天缓存命中率 %（-1 = 无用量数据）
}

// GetTokenCaps returns the per-token caps for every enabled token,
// filling defaults (¥20, locked) for tokens without an explicit cap row.
func (s *QuotaRefreshService) GetTokenCaps() ([]TokenCap, error) {
	mainDB := database.Get()
	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return nil, err
	}

	// saved caps
	saved := map[int64]TokenCap{}
	capRows, err := db.QueryContext(ctx,
		`SELECT token_id, cap_yuan, unlocked FROM quota_refresh_token_caps`)
	if err != nil {
		return nil, err
	}
	defer capRows.Close()
	for capRows.Next() {
		var c TokenCap
		var unlocked int64
		if err := capRows.Scan(&c.TokenID, &c.CapYuan, &unlocked); err != nil {
			return nil, err
		}
		c.Unlocked = unlocked == 1
		saved[c.TokenID] = c
	}

	// every enabled token (name + current remaining quota for display)
	nowUnix := time.Now().Unix()
	query := mainDB.RebindQuery(`
		SELECT id, name, remain_quota, unlimited_quota FROM tokens
		WHERE status = 1 AND deleted_at IS NULL
		  AND (expired_time <= 0 OR expired_time > ?)`)
	rows, err := mainDB.Query(query, nowUnix)
	if err != nil {
		return nil, err
	}
	result := []TokenCap{}
	for _, row := range rows {
		id := toInt64(row["id"])
		c, ok := saved[id]
		if !ok {
			c = TokenCap{TokenID: id, TokenName: toString(row["name"]), CapYuan: DefaultCapYuan}
		} else {
			c.TokenName = toString(row["name"])
		}
		c.RemainQuota = toInt64(row["remain_quota"])
		// unlimited_quota 是 PG BOOLEAN 列，toInt64 不支持 bool，需单独断言
		if b, ok := row["unlimited_quota"].(bool); ok {
			c.Unlimited = b
		}
		result = append(result, c)
	}

	// 当天（本地 0 点起）每个令牌的使用量：剩余 = 上限 - 当天用量
	logDB := database.GetLog()
	if logDB == nil {
		logDB = mainDB
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	if len(result) > 0 {
		placeholders := make([]string, 0, len(result))
		args := make([]interface{}, 0, len(result))
		for _, c := range result {
			placeholders = append(placeholders, "?")
			args = append(args, c.TokenID)
		}
		// cache_tokens 存在 logs.other JSON 中，按引擎提取聚合。
		// 口径说明：OpenAI 兼容路径的 prompt_tokens 含命中（总输入），
		// Claude Messages 路径的 prompt_tokens 不含命中（仅未命中）——
		// 因此按行判定 miss：prompt >= cache 时 miss = prompt - cache，否则 miss = prompt。
		cacheExpr := `(other::jsonb ->> 'cache_tokens')::bigint`
		if logDB != nil && !logDB.IsPG {
			cacheExpr = `CAST(JSON_UNQUOTE(JSON_EXTRACT(other, '$.cache_tokens')) AS SIGNED)`
		}
		usedQuery := logDB.RebindQuery(fmt.Sprintf(
			`SELECT token_id,
			        COALESCE(SUM(quota),0) AS quota,
			        COALESCE(SUM(%s),0) AS cache_tokens,
			        COALESCE(SUM(CASE WHEN prompt_tokens >= %s THEN prompt_tokens - %s ELSE prompt_tokens END),0) AS miss_tokens
			 FROM logs
			 WHERE type = 2 AND created_at >= ? AND token_id IN (%s)
			 GROUP BY token_id`, cacheExpr, cacheExpr, cacheExpr, strings.Join(placeholders, ",")))
		usedArgs := append([]interface{}{dayStart}, args...)
		usedRows, err := logDB.Query(usedQuery, usedArgs...)
		if err != nil {
			return nil, err
		}
		for _, row := range usedRows {
			tid := toInt64(row["token_id"])
			for i := range result {
				if result[i].TokenID != tid {
					continue
				}
				result[i].UsedQuota = toInt64(row["quota"])
				hit := toInt64(row["cache_tokens"])
				miss := toInt64(row["miss_tokens"])
				if hit+miss > 0 {
					rate := float64(hit) * 100 / float64(hit+miss)
					result[i].CacheHitRate = math.Round(rate*100) / 100
				} else {
					result[i].CacheHitRate = -1
				}
			}
		}
	}
	return result, nil
}

// UpdateTokenCaps upserts per-token caps and applies the unlock/lock switch
// immediately to the live tokens:
//   - unlocked: tokens -> unlimited_quota=true (draw from the shared pool)
//   - locked:   tokens -> unlimited_quota=false + remain_quota=cap (cap takes
//     effect right away; the daily reset re-applies the cap every day)
func (s *QuotaRefreshService) UpdateTokenCaps(items []TokenCap) (int, error) {
	if len(items) == 0 {
		return 0, fmt.Errorf("上限配置为空")
	}
	mainDB := database.Get()
	db, err := s.openStore()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return 0, err
	}

	nowUnix := time.Now().Unix()
	touched := 0

	// 当天（本地 0 点起）已用量：锁定令牌的 remain = 上限 − 当天已用
	logDB := database.GetLog()
	if logDB == nil {
		logDB = mainDB
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	usedToday := map[int64]int64{}
	{
		ids := []interface{}{}
		for _, item := range items {
			if item.TokenID > 0 && !item.Unlocked {
				ids = append(ids, item.TokenID)
			}
		}
		if len(ids) > 0 {
			placeholders := make([]string, 0, len(ids))
			for range ids {
				placeholders = append(placeholders, "?")
			}
			usedQuery := logDB.RebindQuery(fmt.Sprintf(
				`SELECT token_id, SUM(quota) FROM logs
				 WHERE type = 2 AND created_at >= ? AND token_id IN (%s)
				 GROUP BY token_id`, strings.Join(placeholders, ",")))
			usedArgs := append([]interface{}{dayStart}, ids...)
			usedRows, err := logDB.Query(usedQuery, usedArgs...)
			if err != nil {
				return 0, err
			}
			for _, row := range usedRows {
				usedToday[toInt64(row["token_id"])] = toInt64(row["sum"])
			}
		}
	}

	for _, item := range items {
		if item.TokenID <= 0 {
			continue
		}
		if item.CapYuan < 0 {
			return touched, fmt.Errorf("上限金额不能为负数")
		}
		unlocked := 0
		if item.Unlocked {
			unlocked = 1
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO quota_refresh_token_caps (token_id, cap_yuan, unlocked, updated_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(token_id) DO UPDATE SET cap_yuan = excluded.cap_yuan, unlocked = excluded.unlocked, updated_at = excluded.updated_at`,
			item.TokenID, item.CapYuan, unlocked, nowUnix); err != nil {
			return touched, err
		}

		// apply unlock/lock immediately on the live token
		if item.Unlocked {
			if _, err := mainDB.Execute(mainDB.RebindQuery(
				`UPDATE tokens SET unlimited_quota = true WHERE id = ?`), item.TokenID); err != nil {
				return touched, err
			}
		} else {
			// 锁定：剩余 = 上限 − 当天已用量（当天已消耗的部分从额度中扣除）
			capQuota := int64(item.CapYuan * 500000)
			remain := capQuota - usedToday[item.TokenID]
			if remain < 0 {
				remain = 0
			}
			if _, err := mainDB.Execute(mainDB.RebindQuery(
				`UPDATE tokens SET unlimited_quota = false, remain_quota = ? WHERE id = ?`),
				remain, item.TokenID); err != nil {
				return touched, err
			}
		}
		touched++
	}
	return touched, nil
}

// NewQuotaRefreshService creates the service
// NOTE: must use config.Get() — config.Load() regenerates the random JWT secret
// on every call and would invalidate all existing tokens.
func NewQuotaRefreshService() *QuotaRefreshService {
	return &QuotaRefreshService{cfg: config.Get()}
}

func (s *QuotaRefreshService) storePath() string {
	dataDir := strings.TrimSpace(s.cfg.DataDir)
	if dataDir == "" {
		dataDir = "./data"
	}
	return filepath.Join(dataDir, "quota-refresh.db")
}

func (s *QuotaRefreshService) openStore() (*sql.DB, error) {
	path := s.storePath()
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func ensureQuotaRefreshTables(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS quota_refresh_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			enabled INTEGER NOT NULL DEFAULT 0,
			quota_amount INTEGER NOT NULL DEFAULT 0,
			refresh_time TEXT NOT NULL DEFAULT '00:00',
			last_run_at INTEGER NOT NULL DEFAULT 0,
			last_run_info TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS quota_refresh_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_at INTEGER NOT NULL DEFAULT 0,
			triggered INTEGER NOT NULL DEFAULT 0,
			quota_amount INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'success',
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS quota_refresh_assignments (
			token_id INTEGER PRIMARY KEY,
			token_name TEXT NOT NULL DEFAULT '',
			quota INTEGER NOT NULL DEFAULT 0,
			share REAL NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS quota_refresh_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			assignment_budget REAL NOT NULL DEFAULT 0,
			assignment_basis TEXT NOT NULL DEFAULT '',
			assignment_updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS quota_refresh_token_caps (
			token_id INTEGER PRIMARY KEY,
			cap_yuan REAL NOT NULL DEFAULT 20,
			unlocked INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS pool_guard_disabled_tokens (
			token_id INTEGER PRIMARY KEY,
			disabled_at INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	// column migration: add mode/shared_budget to an existing settings table
	columns, err := db.QueryContext(ctx, `PRAGMA table_info(quota_refresh_settings)`)
	if err != nil {
		return err
	}
	hasMode, hasSharedBudget := false, false
	for columns.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := columns.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			columns.Close()
			return err
		}
		if name == "mode" {
			hasMode = true
		}
		if name == "shared_budget" {
			hasSharedBudget = true
		}
	}
	columns.Close()
	if !hasMode {
		if _, err := db.ExecContext(ctx, `ALTER TABLE quota_refresh_settings ADD COLUMN mode TEXT NOT NULL DEFAULT 'uniform'`); err != nil {
			return err
		}
	}
	if !hasSharedBudget {
		if _, err := db.ExecContext(ctx, `ALTER TABLE quota_refresh_settings ADD COLUMN shared_budget REAL NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

// GetConfig returns the persisted config (defaults if never saved)
func (s *QuotaRefreshService) GetConfig() (QuotaRefreshConfig, error) {
	cfg := QuotaRefreshConfig{Enabled: false, QuotaAmount: 0, RefreshTime: "00:00", Mode: "uniform", SharedBudget: 0}
	mainDB := database.Get()
	db, err := s.openStore()
	if err != nil {
		return cfg, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return cfg, err
	}
	var enabled, quotaAmount, lastRunAt, updatedAt int64
	var refreshTime, lastRunInfo, mode string
	var sharedBudget float64
	err = db.QueryRowContext(ctx,
		`SELECT enabled, quota_amount, refresh_time, mode, shared_budget, last_run_at, last_run_info, updated_at
		 FROM quota_refresh_settings WHERE id = 1`).
		Scan(&enabled, &quotaAmount, &refreshTime, &mode, &sharedBudget, &lastRunAt, &lastRunInfo, &updatedAt)
	if err == sql.ErrNoRows {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if mode == "" {
		mode = "uniform"
	}
	// 共享池当前剩余：拥有启用令牌的用户钱包余额合计（shared 模式下即池子剩余量）
	walletBalance := int64(0)
	{
		nowUnix := time.Now().Unix()
		usersQuery := mainDB.RebindQuery(`
			SELECT DISTINCT user_id FROM tokens
			WHERE status = 1 AND deleted_at IS NULL
			  AND (expired_time <= 0 OR expired_time > ?)`)
		userRows, err := mainDB.Query(usersQuery, nowUnix)
		if err != nil {
			return cfg, err
		}
		userIDs := []int64{}
		for _, row := range userRows {
			userIDs = append(userIDs, toInt64(row["user_id"]))
		}
		if len(userIDs) > 0 {
			placeholders := make([]string, 0, len(userIDs))
			args := make([]interface{}, 0, len(userIDs))
			for _, id := range userIDs {
				placeholders = append(placeholders, "?")
				args = append(args, id)
			}
			balanceQuery := mainDB.RebindQuery(fmt.Sprintf(
				`SELECT COALESCE(SUM(quota), 0) FROM users WHERE id IN (%s)`, strings.Join(placeholders, ",")))
			balanceRows, err := mainDB.Query(balanceQuery, args...)
			if err != nil {
				return cfg, err
			}
			if len(balanceRows) > 0 {
				walletBalance = toInt64(balanceRows[0]["coalesce"])
			}
		}
	}
	return QuotaRefreshConfig{
		Enabled:       enabled == 1,
		QuotaAmount:   quotaAmount,
		RefreshTime:   refreshTime,
		Mode:          mode,
		SharedBudget:  sharedBudget,
		LastRunAt:     lastRunAt,
		LastRunInfo:   lastRunInfo,
		UpdatedAt:     updatedAt,
		WalletBalance: walletBalance,
	}, nil
}

// UpdateConfig saves the settings (single row id=1, upsert)
// mode: per-token(每令牌额度：统一设置或分配表) | shared(全站共享总额度)
// 'uniform' is accepted as a legacy alias of per-token without an assignment table.
func (s *QuotaRefreshService) UpdateConfig(enabled bool, quotaAmount int64, refreshTime, mode string, sharedBudget float64) (QuotaRefreshConfig, error) {
	if mode == "" || mode == "uniform" {
		mode = "per-token"
	}
	if mode != "per-token" && mode != "shared" {
		return QuotaRefreshConfig{}, fmt.Errorf("额度模式无效")
	}
	if quotaAmount < 0 {
		return QuotaRefreshConfig{}, fmt.Errorf("额度不能为负数")
	}
	if sharedBudget < 0 {
		return QuotaRefreshConfig{}, fmt.Errorf("共享总额度不能为负数")
	}
	// validate HH:mm
	if _, err := time.Parse("15:04", refreshTime); err != nil {
		return QuotaRefreshConfig{}, fmt.Errorf("刷新时间格式无效（应为 HH:mm，如 00:00）")
	}

	db, err := s.openStore()
	if err != nil {
		return QuotaRefreshConfig{}, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return QuotaRefreshConfig{}, err
	}

	if enabled {
		if mode == "shared" && sharedBudget <= 0 {
			return QuotaRefreshConfig{}, fmt.Errorf("开启共享额度前请先设置总预算（元）")
		}
		if mode == "per-token" && quotaAmount == 0 {
			// 统一额度为空时，允许分配表模式（按每令牌分配表执行）
			hasAsg, err := s.hasAssignments(db)
			if err != nil {
				return QuotaRefreshConfig{}, err
			}
			if !hasAsg {
				return QuotaRefreshConfig{}, fmt.Errorf("开启定时器前请设置统一额度，或先生成每令牌分配表")
			}
		}
	}

	now := time.Now().Unix()
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	// keep last_run_at when re-saving so the "already ran today" state survives
	_, err = db.ExecContext(ctx, `
		INSERT INTO quota_refresh_settings (id, enabled, quota_amount, refresh_time, mode, shared_budget, last_run_at, last_run_info, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, COALESCE((SELECT last_run_at FROM quota_refresh_settings WHERE id = 1), 0),
		        COALESCE((SELECT last_run_info FROM quota_refresh_settings WHERE id = 1), ''), ?)
		ON CONFLICT(id) DO UPDATE SET
			enabled = excluded.enabled,
			quota_amount = excluded.quota_amount,
			refresh_time = excluded.refresh_time,
			mode = excluded.mode,
			shared_budget = excluded.shared_budget,
			updated_at = excluded.updated_at`,
		enabledInt, quotaAmount, refreshTime, mode, sharedBudget, now)
	if err != nil {
		return QuotaRefreshConfig{}, err
	}
	return s.GetConfig()
}

// RunNow executes one refresh immediately (manual trigger or scheduled fire).
// mode shared: reset the shared user balance to the daily budget (all tokens unlimited,
//   NewAPI bills unlimited tokens from the user wallet, so the pool is shared across all tokens)
// mode per-token: apply the assignment table; otherwise uniform quota.
func (s *QuotaRefreshService) RunNow() (map[string]interface{}, error) {
	cfg, err := s.GetConfig()
	if err != nil {
		return nil, err
	}

	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return nil, err
	}
	nowUnix := time.Now().Unix()

	// shared：共享额度池（用完都停）
	if cfg.Mode == "shared" {
		return s.runShared(ctx, db, cfg.SharedBudget, nowUnix)
	}

	// per-token：分配表优先，无分配表则统一额度
	assignments, err := s.getAssignmentsInternal(db)
	if err != nil {
		return nil, err
	}
	if len(assignments) > 0 {
		return s.runWithAssignments(ctx, db, assignments, nowUnix)
	}

	if cfg.QuotaAmount <= 0 {
		return nil, fmt.Errorf("未设置额度大小，无法刷新")
	}

	// Reset all enabled tokens: status=1, not deleted, not expired
	mainDB := database.Get()
	updateQuery := mainDB.RebindQuery(`
		UPDATE tokens SET remain_quota = ?, unlimited_quota = false
		WHERE status = 1 AND deleted_at IS NULL
		  AND (expired_time <= 0 OR expired_time > ?)`)
	triggered, err := mainDB.Execute(updateQuery, cfg.QuotaAmount, nowUnix)
	if err != nil {
		return nil, err
	}

	detail := fmt.Sprintf("已重置 %d 个令牌额度为 %d", triggered, cfg.QuotaAmount)
	status := "success"
	if triggered == 0 {
		status = "noop"
		detail = "没有可重置的令牌（全部禁用或已过期）"
	}
	// persist run record + last_run_at in the local store
	_, err = db.ExecContext(ctx,
		`INSERT INTO quota_refresh_runs (run_at, triggered, quota_amount, status, detail) VALUES (?, ?, ?, ?, ?)`,
		nowUnix, triggered, cfg.QuotaAmount, status, detail)
	if err != nil {
		logger.L.Error("定时刷新运行记录写入失败: " + err.Error(), logger.CatBusiness)
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE quota_refresh_settings SET last_run_at = ?, last_run_info = ? WHERE id = 1`,
		nowUnix, detail)

	result := map[string]interface{}{
		"triggered": triggered,
		"quota":     cfg.QuotaAmount,
		"mode":      "uniform",
		"status":    status,
		"detail":    detail,
		"run_at":    nowUnix,
	}
	logger.L.System(fmt.Sprintf("[额度定时刷新] %s", detail))
	return result, nil
}

// QuotaAssignment is one token's daily quota in the per-token table
type QuotaAssignment struct {
	TokenID   int64   `json:"token_id"`
	TokenName string  `json:"token_name"`
	Quota     int64   `json:"quota"`
	Share     float64 `json:"share"`
}

// runShared resets the shared daily budget:
//   - the wallet of every user owning an enabled token is reset to the daily
//     budget (all tokens share this pool; when it runs out NewAPI rejects
//     requests with 402 until the next daily reset)
//   - per-token caps (default ¥20) are applied: capped tokens get
//     remain_quota = cap (unlimited_quota=false) and stop on their own when
//     exhausted; tokens whose cap is unlocked stay unlimited and draw from
//     the shared pool
func (s *QuotaRefreshService) runShared(ctx context.Context, db *sql.DB, budgetYuan float64, nowUnix int64) (map[string]interface{}, error) {
	if budgetYuan <= 0 {
		return nil, fmt.Errorf("未设置共享总额度（元），无法刷新")
	}
	mainDB := database.Get()
	budgetQuota := int64(budgetYuan * 500000)

	// 0) 恢复被共享池耗尽保护（EnforceSharedPoolGuard）禁用的令牌
	//    池子已重新充值，之前因耗尽被 status=2 禁用的令牌恢复启用；
	//    用户手动禁用的令牌（不在记录表中）不受影响。
	{
		guardRows, err := db.QueryContext(ctx,
			`SELECT token_id FROM pool_guard_disabled_tokens`)
		if err != nil {
			return nil, err
		}
		guardIDs := make([]int64, 0)
		for guardRows.Next() {
			var id int64
			if err := guardRows.Scan(&id); err != nil {
				guardRows.Close()
				return nil, err
			}
			guardIDs = append(guardIDs, id)
		}
		guardRows.Close()
		if len(guardIDs) > 0 {
			placeholders := make([]string, 0, len(guardIDs))
			args := make([]interface{}, 0, len(guardIDs))
			for _, id := range guardIDs {
				placeholders = append(placeholders, "?")
				args = append(args, id)
			}
			args = append(args, nowUnix)
			if _, err := mainDB.Execute(mainDB.RebindQuery(fmt.Sprintf(`
				UPDATE tokens SET status = 1
				WHERE id IN (%s) AND deleted_at IS NULL
				  AND (expired_time <= 0 OR expired_time > ?)`, strings.Join(placeholders, ","))), args...); err != nil {
				return nil, err
			}
			if _, err := db.ExecContext(ctx, `DELETE FROM pool_guard_disabled_tokens`); err != nil {
				return nil, err
			}
		}
	}

	// per-token caps from the local store (default ¥20, locked)
	capsMap := map[int64]TokenCap{}
	capRows, err := db.QueryContext(ctx,
		`SELECT token_id, cap_yuan, unlocked FROM quota_refresh_token_caps`)
	if err != nil {
		return nil, err
	}
	for capRows.Next() {
		var c TokenCap
		var unlocked int64
		if err := capRows.Scan(&c.TokenID, &c.CapYuan, &unlocked); err != nil {
			capRows.Close()
			return nil, err
		}
		c.Unlocked = unlocked == 1
		capsMap[c.TokenID] = c
	}
	capRows.Close()

	// 每次共享池充值（runShared）时，把每人上限列表中的所有“已解除”重置为“已限制”：
	// 解除限制意味着走共享池不限个人额度，充值后统一恢复限额，防止个别人持续消耗池子。
	// 同时更新内存中的 capsMap（unlocked 全部置 false）与 SQLite 持久化。
	resetUnlocked := 0
	for tid, c := range capsMap {
		if c.Unlocked {
			resetUnlocked++
			c.Unlocked = false
			capsMap[tid] = c
		}
	}
	if resetUnlocked > 0 {
		if _, err := db.ExecContext(ctx,
			`UPDATE quota_refresh_token_caps SET unlocked = 0`); err != nil {
			return nil, err
		}
	}

	// 1) apply per-token caps: 充值前所有“已解除”已重置为限制，全部令牌统一设上限
	//    （remain_quota = 上限，unlimited_quota = false，用尽即停）
	capQuery := mainDB.RebindQuery(`
		SELECT id FROM tokens
		WHERE status = 1 AND deleted_at IS NULL
		  AND (expired_time <= 0 OR expired_time > ?)`)
	capTokenRows, err := mainDB.Query(capQuery, nowUnix)
	if err != nil {
		return nil, err
	}
	cappedCount := int64(0)
	for _, row := range capTokenRows {
		tid := toInt64(row["id"])
		c, ok := capsMap[tid]
		if !ok {
			c = TokenCap{TokenID: tid, CapYuan: DefaultCapYuan}
		}
		capQuota := int64(c.CapYuan * 500000)
		if capQuota < 0 {
			capQuota = 0
		}
		if _, err := mainDB.Execute(mainDB.RebindQuery(
			`UPDATE tokens SET remain_quota = ?, unlimited_quota = false WHERE id = ?`), capQuota, tid); err != nil {
			return nil, err
		}
		cappedCount++
	}

	// 2) reset the wallet of every user owning an enabled token to the shared budget
	usersQuery := mainDB.RebindQuery(`
		SELECT DISTINCT user_id FROM tokens
		WHERE status = 1 AND deleted_at IS NULL
		  AND (expired_time <= 0 OR expired_time > ?)`)
	rows, err := mainDB.Query(usersQuery, nowUnix)
	if err != nil {
		return nil, err
	}
	userIDs := []int64{}
	for _, row := range rows {
		userIDs = append(userIDs, toInt64(row["user_id"]))
	}
	if len(userIDs) == 0 {
		detail := "没有启用中的令牌，无法重置共享额度"
		_, _ = db.ExecContext(ctx,
			`INSERT INTO quota_refresh_runs (run_at, triggered, quota_amount, status, detail) VALUES (?, ?, ?, ?, ?)`,
			nowUnix, 0, budgetQuota, "noop", detail)
		_, _ = db.ExecContext(ctx,
			`UPDATE quota_refresh_settings SET last_run_at = ?, last_run_info = ? WHERE id = 1`,
			nowUnix, detail)
		return map[string]interface{}{"triggered": 0, "quota": budgetQuota, "mode": "shared", "status": "noop", "detail": detail, "run_at": nowUnix}, nil
	}

	placeholders := make([]string, 0, len(userIDs))
	args := make([]interface{}, 0, len(userIDs))
	for _, id := range userIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	balanceQuery := mainDB.RebindQuery(fmt.Sprintf(
		`UPDATE users SET quota = ? WHERE id IN (%s)`, strings.Join(placeholders, ",")))
	resetArgs := append([]interface{}{budgetQuota}, args...)
	if _, err := mainDB.Execute(balanceQuery, resetArgs...); err != nil {
		return nil, err
	}

	// 记录实际应用的上限（caps 表每个令牌的 cap_yuan；无记录用默认值）——
	// 用于运行记录的文案展示，避免硬编码 DefaultCapYuan 造成“20/50”显示误导
	appliedCapYuan := DefaultCapYuan
	if len(capsMap) > 0 {
		// 取第一个令牌的上限（同批次通常一致；不一致时展示平均值）
		total := 0.0
		for _, c := range capsMap {
			total += c.CapYuan
		}
		appliedCapYuan = total / float64(len(capsMap))
	}
	detail := fmt.Sprintf("共享总额度 ¥%.2f 已重置（%d 个用户池子）；每人上限 ¥%.2f：%d 个令牌限额"+func() string {
		if resetUnlocked > 0 {
			return fmt.Sprintf("，%d 个已解除已重置为限制", resetUnlocked)
		}
		return ""
	}(),
		budgetYuan, len(userIDs), appliedCapYuan, cappedCount)
	status := "success"
	_, err = db.ExecContext(ctx,
		`INSERT INTO quota_refresh_runs (run_at, triggered, quota_amount, status, detail) VALUES (?, ?, ?, ?, ?)`,
		nowUnix, len(userIDs), budgetQuota, status, detail)
	if err != nil {
		logger.L.Error("定时刷新运行记录写入失败: "+err.Error(), logger.CatBusiness)
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE quota_refresh_settings SET last_run_at = ?, last_run_info = ? WHERE id = 1`,
		nowUnix, detail)

	result := map[string]interface{}{
		"triggered": len(userIDs),
		"quota":     budgetQuota,
		"mode":      "shared",
		"status":    status,
		"detail":    detail,
		"run_at":    nowUnix,
	}
	logger.L.System(fmt.Sprintf("[额度定时刷新] %s", detail))
	return result, nil
}

// runWithAssignments resets each token to its own per-token quota
func (s *QuotaRefreshService) runWithAssignments(ctx context.Context, db *sql.DB, assignments []QuotaAssignment, nowUnix int64) (map[string]interface{}, error) {
	triggered := int64(0)
	totalAssigned := int64(0)
	mainDB := database.Get()
	for _, a := range assignments {
		if a.Quota <= 0 {
			continue
		}
		updateQuery := mainDB.RebindQuery(`
			UPDATE tokens SET remain_quota = ?, unlimited_quota = false
			WHERE id = ? AND status = 1 AND deleted_at IS NULL
			  AND (expired_time <= 0 OR expired_time > ?)`)
		n, err := mainDB.Execute(updateQuery, a.Quota, a.TokenID, nowUnix)
		if err != nil {
			logger.L.Error(fmt.Sprintf("[额度定时刷新] 令牌 %d(%s) 更新失败: %v", a.TokenID, a.TokenName, err), logger.CatTask)
			continue
		}
		triggered += n
		totalAssigned += a.Quota
	}

	detail := fmt.Sprintf("按分配表重置 %d 个令牌，合计额度 %d", triggered, totalAssigned)
	status := "success"
	if triggered == 0 {
		status = "noop"
		detail = "没有可重置的令牌（全部禁用或已过期）"
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO quota_refresh_runs (run_at, triggered, quota_amount, status, detail) VALUES (?, ?, ?, ?, ?)`,
		nowUnix, triggered, totalAssigned, status, detail)
	if err != nil {
		logger.L.Error("定时刷新运行记录写入失败: "+err.Error(), logger.CatBusiness)
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE quota_refresh_settings SET last_run_at = ?, last_run_info = ? WHERE id = 1`,
		nowUnix, detail)

	result := map[string]interface{}{
		"triggered": triggered,
		"quota":     totalAssigned,
		"mode":      "per-token",
		"status":    status,
		"detail":    detail,
		"run_at":    nowUnix,
	}
	logger.L.System(fmt.Sprintf("[额度定时刷新] %s", detail))
	return result, nil
}

// MaybeRun checks the clock and fires the refresh when the configured time is due
// and today's run has not happened yet. Called by the background ticker every minute.
func (s *QuotaRefreshService) MaybeRun(now time.Time) error {
	cfg, err := s.GetConfig()
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.QuotaAmount <= 0 {
		return nil
	}
	if now.Format("15:04") != cfg.RefreshTime {
		return nil
	}
	// skip if already ran on this calendar day
	if cfg.LastRunAt > 0 && sameDay(cfg.LastRunAt, now.Unix()) {
		return nil
	}
	_, err = s.RunNow()
	return err
}

// EnforceSharedPoolGuard: 共享总额度模式下，当共享池（拥有启用令牌的用户钱包合计）
// 余额耗尽（<=0）时，把所有启用令牌禁用（status=2）并清零剩余额度，使全站停用；
// 即使个别令牌自身还有余额也不允许继续使用。
// 注意：new-api 的 Redis 令牌缓存（TTL 60s）+ 扣费回写会覆盖 remain_quota/unlimited_quota
// 的直改 DB 值，但不会覆盖 status，所以用禁用（status=2）保证可靠停用。
// 被禁用的令牌 id 记录在 SQLite pool_guard_disabled_tokens，由 runShared 恢复。
func (s *QuotaRefreshService) EnforceSharedPoolGuard() error {
	cfg, err := s.GetConfig()
	if err != nil {
		return err
	}
	// 仅共享总额度模式且启用时生效
	if cfg.Mode != "shared" || !cfg.Enabled {
		return nil
	}
	if cfg.WalletBalance > 0 {
		return nil // 池子还有余额，不干预
	}

	mainDB := database.Get()
	nowUnix := time.Now().Unix()

	// 找出当前启用的令牌（将被禁用）
	enabledQuery := mainDB.RebindQuery(`
		SELECT id FROM tokens
		WHERE status = 1 AND deleted_at IS NULL
		  AND (expired_time <= 0 OR expired_time > ?)`)
	rows, err := mainDB.Query(enabledQuery, nowUnix)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, toInt64(row["id"]))
	}

	// 禁用并清零剩余额度（status 不会被 new-api 回写覆盖，禁用可靠生效）
	affected, err := mainDB.Execute(mainDB.RebindQuery(`
		UPDATE tokens
		SET status = 2, remain_quota = 0, unlimited_quota = false
		WHERE status = 1 AND deleted_at IS NULL
		  AND (expired_time <= 0 OR expired_time > ?)`), nowUnix)
	if err != nil {
		return err
	}

	// 记录被禁用的令牌 id，供 runShared 恢复（区分用户手动禁用的令牌）
	if len(ids) > 0 {
		db, err := s.openStore()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ensureQuotaRefreshTables(ctx, db); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := db.ExecContext(ctx,
				`INSERT OR IGNORE INTO pool_guard_disabled_tokens (token_id, disabled_at) VALUES (?, ?)`,
				id, time.Now().Unix()); err != nil {
				return err
			}
		}
	}

	logger.L.System(fmt.Sprintf(
		"[额度共享池] 共享池余额已耗尽（¥%.2f），已停用 %d 个令牌（禁用+剩余额度清零）",
		float64(cfg.WalletBalance)/500000, affected))
	return nil
}

// GetRuns returns recent refresh history (most recent first)
func (s *QuotaRefreshService) GetRuns(limit int) ([]QuotaRefreshRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, run_at, triggered, quota_amount, status, detail
		 FROM quota_refresh_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []QuotaRefreshRun{}
	for rows.Next() {
		var r QuotaRefreshRun
		if err := rows.Scan(&r.ID, &r.RunAt, &r.Triggered, &r.QuotaAmount, &r.Status, &r.Detail); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// getAssignmentsInternal reads the per-token assignment table
// hasAssignments reports whether a per-token assignment table is populated.
func (s *QuotaRefreshService) hasAssignments(db *sql.DB) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_refresh_assignments`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *QuotaRefreshService) getAssignmentsInternal(db *sql.DB) ([]QuotaAssignment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx,
		`SELECT token_id, token_name, quota, share FROM quota_refresh_assignments ORDER BY quota DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assignments := []QuotaAssignment{}
	for rows.Next() {
		var a QuotaAssignment
		if err := rows.Scan(&a.TokenID, &a.TokenName, &a.Quota, &a.Share); err != nil {
			return nil, err
	}
		assignments = append(assignments, a)
	}
	return assignments, rows.Err()
}

// GetAssignments returns the current per-token assignment table + meta
func (s *QuotaRefreshService) GetAssignments() (map[string]interface{}, error) {
	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return nil, err
	}
	assignments, err := s.getAssignmentsInternal(db)
	if err != nil {
		return nil, err
	}
	meta := struct {
		Budget       float64 `json:"budget"`
		Basis        string  `json:"basis"`
		UpdatedAt    int64   `json:"updated_at"`
	}{}
	err = db.QueryRowContext(ctx,
		`SELECT assignment_budget, assignment_basis, assignment_updated_at
		 FROM quota_refresh_meta WHERE id = 1`).Scan(&meta.Budget, &meta.Basis, &meta.UpdatedAt)
	if err == sql.ErrNoRows {
		meta.Budget = 0
		meta.Basis = ""
		meta.UpdatedAt = 0
	} else if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"items":       assignments,
		"budget":      meta.Budget,
		"basis":       meta.Basis,
		"updated_at":  meta.UpdatedAt,
	}, nil
}

// RecommendAssignments computes two-tier quota recommendations from usage shares:
//   big   — quota per token for the high-usage tier (tokens covering ~80% of volume)
//   small — quota per token for the low-usage tier (the rest)
// Both values are editable in the UI; confirming fills the assignment table by tier.
func (s *QuotaRefreshService) RecommendAssignments(budgetYuan float64, days int) (map[string]interface{}, error) {
	if budgetYuan <= 0 {
		return nil, fmt.Errorf("日预算必须大于 0")
	}
	if days <= 0 || days > 90 {
		days = 7
	}
	logDB := database.GetLog()
	if logDB == nil {
		logDB = database.Get()
	}
	mainDB := database.Get()
	nowUnix := time.Now().Unix()

	// 近 N 天每个令牌的 token 用量
	windowStart := nowUnix - int64(days)*86400
	countQuery := logDB.RebindQuery(`
		SELECT token_id, SUM(prompt_tokens + completion_tokens) AS tok
		FROM logs WHERE type = 2 AND created_at >= ? AND token_id > 0
		GROUP BY token_id HAVING SUM(prompt_tokens + completion_tokens) > 0`)
	rows, err := logDB.Query(countQuery, windowStart)
	if err != nil {
		return nil, err
	}
	usage := map[int64]float64{}
	var total float64
	for _, row := range rows {
		tokenID := toInt64(row["token_id"])
		tok := toFloat64(row["tok"])
		usage[tokenID] = tok
		total += tok
	}
	if total <= 0 {
		return nil, fmt.Errorf("统计窗口内没有使用记录，无法生成推荐")
	}

	// 令牌名
	names := map[int64]string{}
	placeholders := make([]string, 0, len(usage))
	ids := make([]interface{}, 0, len(usage))
	for id := range usage {
		placeholders = append(placeholders, "?")
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		nameQuery := mainDB.RebindQuery(fmt.Sprintf(
			`SELECT id, name FROM tokens WHERE id IN (%s)`, strings.Join(placeholders, ",")))
		nameRows, err := mainDB.Query(nameQuery, ids...)
		if err != nil {
			return nil, err
		}
		for _, row := range nameRows {
			names[toInt64(row["id"])] = toString(row["name"])
		}
	}

	// 按用量降序排序
	type item struct {
		id   int64
		name string
		tok  float64
	}
	sorted := make([]item, 0, len(usage))
	for id, tok := range usage {
		sorted = append(sorted, item{id: id, name: names[id], tok: tok})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].tok > sorted[j].tok })

	// 高用量档：累计用量前 80%（tierSplit）的令牌
	const tierSplit = 0.8
	var acc float64
	bigTokens := []item{}
	smallTokens := []item{}
	for _, it := range sorted {
		if acc < total*tierSplit {
			acc += it.tok
			bigTokens = append(bigTokens, it)
		} else {
			smallTokens = append(smallTokens, it)
		}
	}

	const floorQuota = int64(100000) // ¥0.2 下限
	// 零用量令牌：近 N 天没有调用记录的启用令牌。固定给最低额度（¥0.2/天）
	// 并纳入分配表，防止突发使用突破预算；其总额先从预算中扣除，不摊薄有用量令牌
	idleTokens := []item{}
	{
		idlePlaceholders := make([]string, 0, len(usage))
		idleIDs := make([]interface{}, 0, len(usage))
		for id := range usage {
			idlePlaceholders = append(idlePlaceholders, "?")
			idleIDs = append(idleIDs, id)
		}
		idleQuery := mainDB.RebindQuery(fmt.Sprintf(
			`SELECT id, name FROM tokens WHERE status = 1 AND deleted_at IS NULL AND id NOT IN (%s)`, strings.Join(idlePlaceholders, ",")))
		idleRows, err := mainDB.Query(idleQuery, idleIDs...)
		if err != nil {
			return nil, err
		}
		for _, row := range idleRows {
			idleTokens = append(idleTokens, item{id: toInt64(row["id"]), name: toString(row["name"])})
		}
		sort.Slice(idleTokens, func(i, j int) bool { return idleTokens[i].id < idleTokens[j].id })
	}

	// 预算扣除零用量令牌最低额度后，再按占比分配 big/small
	idleYuan := float64(floorQuota) / 500000 // ¥0.2/天
	if len(idleTokens) > 0 && float64(len(idleTokens))*idleYuan > budgetYuan {
		// 预算不足以覆盖所有零用量令牌的最低额度：全部分给零用量令牌
		idleYuan = budgetYuan / float64(len(idleTokens))
	}
	effectiveBudget := budgetYuan - float64(len(idleTokens))*idleYuan
	if effectiveBudget < 0 {
		effectiveBudget = 0
	}
	bigYuan, smallYuan := 0.0, 0.0
	if len(bigTokens) > 0 && effectiveBudget > 0 {
		bigYuan = effectiveBudget * (acc / total) / float64(len(bigTokens))
	}
	if len(smallTokens) > 0 && effectiveBudget > 0 {
		smallYuan = effectiveBudget * ((total - acc) / total) / float64(len(smallTokens))
		if smallYuan < idleYuan {
			smallYuan = idleYuan
		}
	}

	bigIDs := make([]int64, 0, len(bigTokens))
	bigNames := make([]string, 0, len(bigTokens))
	for _, it := range bigTokens {
		bigIDs = append(bigIDs, it.id)
		bigNames = append(bigNames, it.name)
	}
	smallIDs := make([]int64, 0, len(smallTokens))
	smallNames := make([]string, 0, len(smallTokens))
	for _, it := range smallTokens {
		smallIDs = append(smallIDs, it.id)
		smallNames = append(smallNames, it.name)
	}
	idleIDs := make([]int64, 0, len(idleTokens))
	idleNames := make([]string, 0, len(idleTokens))
	for _, it := range idleTokens {
		idleIDs = append(idleIDs, it.id)
		idleNames = append(idleNames, it.name)
	}

	return map[string]interface{}{
		"basis":      fmt.Sprintf("近%d天token用量占比", days),
		"budget":     budgetYuan,
		"big":        map[string]interface{}{"yuan": round2(bigYuan), "quota": int64(bigYuan * 500000), "count": len(bigTokens), "token_ids": bigIDs, "tokens": bigNames},
		"small":      map[string]interface{}{"yuan": round2(smallYuan), "quota": int64(smallYuan * 500000), "count": len(smallTokens), "token_ids": smallIDs, "tokens": smallNames},
		"idle":       map[string]interface{}{"yuan": round2(idleYuan), "quota": int64(idleYuan * 500000), "count": len(idleTokens), "token_ids": idleIDs, "tokens": idleNames},
		"total_yuan": budgetYuan,
	}, nil
}

// GenerateAssignments computes per-token daily quotas:
//   mode="share"   — by usage share over the last N days (token volume prompt+completion,
//                     pricing-independent, unlike quota which is polluted by price changes)
//   mode="average" — budget split evenly across all enabled tokens
func (s *QuotaRefreshService) GenerateAssignments(budgetYuan float64, days int, mode string) (map[string]interface{}, error) {
	if budgetYuan <= 0 {
		return nil, fmt.Errorf("日预算必须大于 0")
	}
	if days <= 0 || days > 90 {
		days = 7
	}
	if mode == "" {
		mode = "share"
	}
	mainDB := database.Get()
	nowUnix := time.Now().Unix()

	const floorQuota = int64(100000) // ¥0.2 下限，避免额度过小

	assignments := []QuotaAssignment{}
	var totalAssigned int64

	if mode == "average" {
		// 总额度平均分配：所有启用中令牌均分
		activeQuery := mainDB.RebindQuery(`SELECT id, name FROM tokens WHERE status = 1 AND deleted_at IS NULL`)
		activeRows, err := mainDB.Query(activeQuery)
		if err != nil {
			return nil, err
		}
		type tokName struct{ id int64; name string }
		activeIDs := []tokName{}
		for _, row := range activeRows {
			activeIDs = append(activeIDs, tokName{id: toInt64(row["id"]), name: toString(row["name"])})
		}
		if len(activeIDs) == 0 {
			return nil, fmt.Errorf("没有启用中的令牌，无法分配")
		}
		perToken := int64(budgetYuan * 500000 / float64(len(activeIDs)))
		if perToken < floorQuota {
			perToken = floorQuota
		}
		for _, t := range activeIDs {
			assignments = append(assignments, QuotaAssignment{
				TokenID:   t.id,
				TokenName: t.name,
				Quota:     perToken,
				Share:     1.0 / float64(len(activeIDs)),
			})
			totalAssigned += perToken
		}
	} else {
		// 按近 N 天 token 用量占比分配（默认）
		logDB := database.GetLog()
		if logDB == nil {
			logDB = database.Get()
		}
		windowStart := nowUnix - int64(days)*86400
		countQuery := logDB.RebindQuery(`
			SELECT token_id, SUM(prompt_tokens + completion_tokens) AS tok
			FROM logs WHERE type = 2 AND created_at >= ? AND token_id > 0
			GROUP BY token_id HAVING SUM(prompt_tokens + completion_tokens) > 0`)
		rows, err := logDB.Query(countQuery, windowStart)
		if err != nil {
			return nil, err
		}
		usage := map[int64]float64{}
		var total float64
		for _, row := range rows {
			tokenID := toInt64(row["token_id"])
			tok := toFloat64(row["tok"])
			usage[tokenID] = tok
			total += tok
		}
		if total <= 0 {
			return nil, fmt.Errorf("统计窗口内没有使用记录，无法生成分配表")
		}

		// 令牌名（logDB 可能与 mainDB 不同库，名字从 mainDB tokens 查）
		names := map[int64]string{}
		placeholders := make([]string, 0, len(usage))
		ids := make([]interface{}, 0, len(usage))
		for id := range usage {
			placeholders = append(placeholders, "?")
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			nameQuery := mainDB.RebindQuery(fmt.Sprintf(
				`SELECT id, name FROM tokens WHERE id IN (%s)`, strings.Join(placeholders, ",")))
			nameRows, err := mainDB.Query(nameQuery, ids...)
			if err != nil {
				return nil, err
			}
			for _, row := range nameRows {
				names[toInt64(row["id"])] = toString(row["name"])
			}
		}

		for tokenID, tok := range usage {
			share := tok / total
			quota := int64(budgetYuan * 500000 * share)
			if quota < floorQuota {
				quota = floorQuota
			}
			assignments = append(assignments, QuotaAssignment{
				TokenID:   tokenID,
				TokenName: names[tokenID],
				Quota:     quota,
				Share:     share,
			})
			totalAssigned += quota
		}
	}

	// persist (replace table + meta)
	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM quota_refresh_assignments`); err != nil {
		return nil, err
	}
	for _, a := range assignments {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO quota_refresh_assignments (token_id, token_name, quota, share) VALUES (?, ?, ?, ?)`,
			a.TokenID, a.TokenName, a.Quota, a.Share); err != nil {
			return nil, err
		}
	}
	modeLabel := "平均分配"
	if mode == "share" {
		modeLabel = fmt.Sprintf("近%d天token用量占比", days)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO quota_refresh_meta (id, assignment_budget, assignment_basis, assignment_updated_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET assignment_budget = excluded.assignment_budget,
		   assignment_basis = excluded.assignment_basis, assignment_updated_at = excluded.assignment_updated_at`,
		budgetYuan, modeLabel, nowUnix); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"items":       assignments,
		"budget":      budgetYuan,
		"basis":       modeLabel,
		"total_quota": totalAssigned,
		"total_yuan":  float64(totalAssigned) / 500000,
		"updated_at":  nowUnix,
	}, nil
}

// UpdateAssignments replaces the per-token assignment table with manually entered values.
// items: [{token_id, token_name, quota}] — quota in quota units (元 * 500000).
func (s *QuotaRefreshService) UpdateAssignments(items []QuotaAssignment) (map[string]interface{}, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("分配表为空")
	}
	mainDB := database.Get()
	// validate token ids exist in main DB and fetch names
	nameQuery := mainDB.RebindQuery(`SELECT id, name FROM tokens WHERE deleted_at IS NULL`)
	nameRows, err := mainDB.Query(nameQuery)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, row := range nameRows {
		names[toInt64(row["id"])] = toString(row["name"])
	}
	for _, a := range items {
		if a.TokenID <= 0 {
			return nil, fmt.Errorf("令牌 ID 无效")
		}
		if a.Quota < 0 {
			return nil, fmt.Errorf("令牌 %d 额度不能为负数", a.TokenID)
		}
	}

	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM quota_refresh_assignments`); err != nil {
		return nil, err
	}
	var totalAssigned int64
	for _, a := range items {
		name := names[a.TokenID]
		if _, err := db.ExecContext(ctx,
			`INSERT INTO quota_refresh_assignments (token_id, token_name, quota, share) VALUES (?, ?, ?, 0)`,
			a.TokenID, name, a.Quota); err != nil {
			return nil, err
		}
		totalAssigned += a.Quota
	}
	basis := "手动设置"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO quota_refresh_meta (id, assignment_budget, assignment_basis, assignment_updated_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET assignment_budget = excluded.assignment_budget,
		   assignment_basis = excluded.assignment_basis, assignment_updated_at = excluded.assignment_updated_at`,
		float64(totalAssigned)/500000, basis, time.Now().Unix()); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"items":       items,
		"budget":      float64(totalAssigned) / 500000,
		"basis":       basis,
		"total_quota": totalAssigned,
		"total_yuan":  float64(totalAssigned) / 500000,
		"updated_at":  time.Now().Unix(),
	}, nil
}

// ClearAssignments empties the per-token assignment table (falls back to uniform quota)
func (s *QuotaRefreshService) ClearAssignments() error {
	db, err := s.openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureQuotaRefreshTables(ctx, db); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM quota_refresh_assignments`)
	return err
}

func sameDay(a, b int64) bool {
	ta := time.Unix(a, 0)
	tb := time.Unix(b, 0)
	return ta.Year() == tb.Year() && ta.YearDay() == tb.YearDay()
}

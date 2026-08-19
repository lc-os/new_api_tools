package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
)

// DeepSeekV4Models are the models whose prices switch between peak/off-peak.
// Peak hours (Beijing time): 09:00-12:00 & 14:00-18:00. The DEFAULT price is
// the off-peak (idle) price; during peak hours the price is multiplied by
// PeakMultiplier (default 2). Only the ModelRatio keys of these models are
// rewritten — new-api bills cache hits as ModelRatio×CacheRatio and output as
// ModelRatio×CompletionRatio, so hit/output scale with ModelRatio automatically.
var DeepSeekV4Models = []string{"deepseek-v4-flash", "deepseek-v4-flash-max", "deepseek-v4-pro", "deepseek-v4-pro-max"}

// PeakPricingService switches new-api model prices between the default
// off-peak price and PeakMultiplier× at peak hours. Config + off-peak price
// baseline live in a local SQLite store (./data/peak-pricing.db).
type PeakPricingService struct {
	cfg *config.Config
}

// PeakPricingConfig is the persisted single-row settings
// 默认按空闲价计费；高峰时段价格 = 空闲价 × PeakMultiplier
// （旧字段 offpeak_ratio 仅用于兼容迁移，新逻辑以 peak_multiplier 为准）
type PeakPricingConfig struct {
	Enabled        bool    `json:"enabled"`
	PeakPeriods    string  `json:"peak_periods"`     // "09:00-12:00,14:00-18:00"
	PeakMultiplier float64 `json:"peak_multiplier"`  // 高峰价格倍数（默认 2）
	CurrentState   string  `json:"current_state"`    // peak | offpeak（上次应用的时段）
	LastSwitchAt   int64   `json:"last_switch_at"`
	UpdatedAt      int64   `json:"updated_at"`
}

// PeakBaseline is the captured off-peak (default) price snapshot of deepseek-v4 keys
type PeakBaseline struct {
	ModelRatio      map[string]float64 `json:"model_ratio"`
	CompletionRatio map[string]float64 `json:"completion_ratio"`
	CacheRatio      map[string]float64 `json:"cache_ratio"`
	CapturedAt      int64              `json:"captured_at"`
}

// NewPeakPricingService creates the service
// NOTE: must use config.Get() — config.Load() regenerates the random JWT secret
// on every call and would invalidate all existing tokens.
func NewPeakPricingService() *PeakPricingService {
	return &PeakPricingService{cfg: config.Get()}
}

func (s *PeakPricingService) storePath() string {
	dataDir := strings.TrimSpace(s.cfg.DataDir)
	if dataDir == "" {
		dataDir = "./data"
	}
	return filepath.Join(dataDir, "peak-pricing.db")
}

func (s *PeakPricingService) openStore() (*sql.DB, error) {
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

func ensurePeakPricingTables(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS peak_pricing_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			enabled INTEGER NOT NULL DEFAULT 0,
			peak_periods TEXT NOT NULL DEFAULT '09:00-12:00,14:00-18:00',
			offpeak_ratio REAL NOT NULL DEFAULT 0.5,
			current_state TEXT NOT NULL DEFAULT 'offpeak',
			last_switch_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS peak_pricing_baseline (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			baseline TEXT NOT NULL DEFAULT '{}',
			captured_at INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	// column migration: add peak_multiplier (peak price = off-peak × multiplier)
	columns, err := db.QueryContext(ctx, `PRAGMA table_info(peak_pricing_settings)`)
	if err != nil {
		return err
	}
	hasMultiplier := false
	for columns.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := columns.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			columns.Close()
			return err
		}
		if name == "peak_multiplier" {
			hasMultiplier = true
		}
	}
	columns.Close()
	if !hasMultiplier {
		if _, err := db.ExecContext(ctx, `ALTER TABLE peak_pricing_settings ADD COLUMN peak_multiplier REAL NOT NULL DEFAULT 2`); err != nil {
			return err
		}
	}
	return nil
}

// GetConfig returns the persisted config (defaults if never saved)
func (s *PeakPricingService) GetConfig() (PeakPricingConfig, error) {
	cfg := PeakPricingConfig{Enabled: false, PeakPeriods: "09:00-12:00,14:00-18:00", PeakMultiplier: 2, CurrentState: "offpeak"}
	db, err := s.openStore()
	if err != nil {
		return cfg, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensurePeakPricingTables(ctx, db); err != nil {
		return cfg, err
	}
	var enabled, lastSwitchAt, updatedAt int64
	var peakPeriods, currentState string
	var offpeakRatio, peakMultiplier float64
	err = db.QueryRowContext(ctx,
		`SELECT enabled, peak_periods, offpeak_ratio, peak_multiplier, current_state, last_switch_at, updated_at
		 FROM peak_pricing_settings WHERE id = 1`).
		Scan(&enabled, &peakPeriods, &offpeakRatio, &peakMultiplier, &currentState, &lastSwitchAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return cfg, nil
		}
		return cfg, err
	}
	cfg.Enabled = enabled != 0
	cfg.PeakPeriods = peakPeriods
	// 兼容旧配置：新逻辑以 peak_multiplier 为准；旧值 0（未迁移/未保存）时由 offpeak_ratio 反推（0.5 → 2）
	if peakMultiplier <= 0 {
		if offpeakRatio > 0 && offpeakRatio <= 1 {
			cfg.PeakMultiplier = 1 / offpeakRatio
		} else {
			cfg.PeakMultiplier = 2
		}
	} else {
		cfg.PeakMultiplier = peakMultiplier
	}
	cfg.CurrentState = currentState
	if cfg.CurrentState == "" {
		cfg.CurrentState = "offpeak"
	}
	cfg.LastSwitchAt = lastSwitchAt
	cfg.UpdatedAt = updatedAt
	return cfg, nil
}

// parseHM parses "HH:MM" into minutes since midnight
func parseHM(s string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// ValidatePeakPeriods checks the "HH:MM-HH:MM,HH:MM-HH:MM" format
func ValidatePeakPeriods(periods string) error {
	trimmed := strings.TrimSpace(periods)
	if trimmed == "" {
		return fmt.Errorf("高峰时间段不能为空")
	}
	for _, p := range strings.Split(trimmed, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		parts := strings.Split(p, "-")
		if len(parts) != 2 {
			return fmt.Errorf("时间段格式无效（应为 HH:MM-HH:MM，多个用逗号分隔）：%s", p)
		}
		start, ok1 := parseHM(parts[0])
		end, ok2 := parseHM(parts[1])
		if !ok1 || !ok2 {
			return fmt.Errorf("时间段格式无效（应为 HH:MM-HH:MM）：%s", p)
		}
		if start >= end {
			return fmt.Errorf("时间段结束时间需晚于开始时间：%s", p)
		}
	}
	return nil
}

// InPeakPeriod reports whether `now` (local/Beijing time) falls in any peak period
func (s *PeakPricingService) InPeakPeriod(periods string, now time.Time) bool {
	for _, p := range strings.Split(periods, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		parts := strings.Split(p, "-")
		if len(parts) != 2 {
			continue
		}
		start, ok1 := parseHM(parts[0])
		end, ok2 := parseHM(parts[1])
		if !ok1 || !ok2 {
			continue
		}
		cur := now.Hour()*60 + now.Minute()
		if start <= cur && cur < end {
			return true
		}
	}
	return false
}

// getOptionJSON reads an options row's JSON value (map) from the main new-api DB
func getOptionJSON(mainDB *database.Manager, key string) (map[string]interface{}, error) {
	q := mainDB.RebindQuery(`SELECT value FROM options WHERE key = ?`)
	rows, err := mainDB.Query(q, key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]interface{}{}, nil
	}
	raw, ok := rows[0]["value"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return map[string]interface{}{}, nil
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("解析 options.%s 失败: %v", key, err)
	}
	return m, nil
}

func setOptionJSON(mainDB *database.Manager, key string, m map[string]interface{}) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	q := mainDB.RebindQuery(`UPDATE options SET value = ? WHERE key = ?`)
	if _, err := mainDB.Execute(q, string(data), key); err != nil {
		return err
	}
	return nil
}

// CaptureBaseline reads the current deepseek-v4 prices from new-api options and
// stores them as the peak-price baseline (启用时把当前定价视为高峰价基准)
func (s *PeakPricingService) CaptureBaseline() (PeakBaseline, error) {
	mainDB := database.Get()
	baseline := PeakBaseline{ModelRatio: map[string]float64{}, CompletionRatio: map[string]float64{}, CacheRatio: map[string]float64{}, CapturedAt: time.Now().Unix()}
	for _, key := range []string{"ModelRatio", "CompletionRatio", "CacheRatio"} {
		m, err := getOptionJSON(mainDB, key)
		if err != nil {
			return baseline, err
		}
		target := baseline.ModelRatio
		if key == "CompletionRatio" {
			target = baseline.CompletionRatio
		} else if key == "CacheRatio" {
			target = baseline.CacheRatio
		}
		for _, model := range DeepSeekV4Models {
			if v, ok := m[model].(float64); ok {
				target[model] = v
			}
		}
	}

	db, err := s.openStore()
	if err != nil {
		return baseline, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensurePeakPricingTables(ctx, db); err != nil {
		return baseline, err
	}
	data, _ := json.Marshal(baseline)
	_, err = db.ExecContext(ctx, `
		INSERT INTO peak_pricing_baseline (id, baseline, captured_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET baseline = excluded.baseline, captured_at = excluded.captured_at`,
		string(data), baseline.CapturedAt)
	return baseline, err
}

// GetBaseline returns the stored peak-price baseline (may be empty if never captured)
func (s *PeakPricingService) GetBaseline() (PeakBaseline, error) {
	var baseline PeakBaseline
	db, err := s.openStore()
	if err != nil {
		return baseline, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensurePeakPricingTables(ctx, db); err != nil {
		return baseline, err
	}
	var raw string
	var capturedAt int64
	err = db.QueryRowContext(ctx, `SELECT baseline, captured_at FROM peak_pricing_baseline WHERE id = 1`).
		Scan(&raw, &capturedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return baseline, nil
		}
		return baseline, err
	}
	if strings.TrimSpace(raw) == "" {
		return baseline, nil
	}
	if err := json.Unmarshal([]byte(raw), &baseline); err != nil {
		return baseline, err
	}
	return baseline, nil
}

// roundPrice rounds to 8 decimal places to keep the JSON tidy
func roundPrice(v float64) float64 {
	return math.Round(v*1e8) / 1e8
}

// applyStateFactor writes deepseek-v4 prices into new-api options.
// price = baseline × factor — baseline is the DEFAULT (off-peak) price,
// factor 1 = off-peak, PeakMultiplier (default 2) = peak hours.
// IMPORTANT: only ModelRatio is scaled. new-api bills output as
// ModelRatio × CompletionRatio and cache hits as ModelRatio × CacheRatio,
// so once ModelRatio is scaled both output and hit prices scale automatically;
// scaling the other ratios too would make them ×(factor²), misaligning with
// DeepSeek's official rates.
func (s *PeakPricingService) applyStateFactor(factor float64) error {
	baseline, err := s.GetBaseline()
	if err != nil {
		return err
	}
	if len(baseline.ModelRatio) == 0 {
		return fmt.Errorf("尚未捕获空闲价基准，请先执行「捕获基准」或启用时段定价")
	}
	mainDB := database.Get()
	sets := []struct {
		key      string
		baseline map[string]float64
		scale    float64
	}{
		{"ModelRatio", baseline.ModelRatio, factor},
		// 输出价 = ModelRatio × CompletionRatio；命中价 = ModelRatio × CacheRatio。
		// ModelRatio 已按 factor 缩放，这两个比例保持基准即可让价格同比例缩放。
		{"CompletionRatio", baseline.CompletionRatio, 1.0},
		{"CacheRatio", baseline.CacheRatio, 1.0},
	}
	for _, set := range sets {
		m, err := getOptionJSON(mainDB, set.key)
		if err != nil {
			return err
		}
		changed := false
		for _, model := range DeepSeekV4Models {
			if v, ok := set.baseline[model]; ok {
				m[model] = roundPrice(v * set.scale)
				changed = true
			}
		}
		if changed {
			if err := setOptionJSON(mainDB, set.key, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// ApplyNow applies pricing for the current time (peak or off-peak) and
// records the applied state
func (s *PeakPricingService) ApplyNow() (map[string]interface{}, error) {
	cfg, err := s.GetConfig()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("时段定价未启用")
	}
	return s.applyState(cfg, time.Now())
}

// applyState applies the pricing for `now` and persists the new state
// 默认（空闲）价 = 基准；高峰价 = 基准 × PeakMultiplier
func (s *PeakPricingService) applyState(cfg PeakPricingConfig, now time.Time) (map[string]interface{}, error) {
	state := "offpeak"
	if s.InPeakPeriod(cfg.PeakPeriods, now) {
		state = "peak"
	}
	factor := 1.0
	if state == "peak" {
		factor = cfg.PeakMultiplier
	}
	if err := s.applyStateFactor(factor); err != nil {
		return nil, err
	}

	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensurePeakPricingTables(ctx, db); err != nil {
		return nil, err
	}
	nowUnix := now.Unix()
	_, err = db.ExecContext(ctx, `
		UPDATE peak_pricing_settings SET current_state = ?, last_switch_at = ?, updated_at = ? WHERE id = 1`,
		state, nowUnix, nowUnix)
	if err != nil {
		return nil, err
	}
	desc := "高峰价（空闲×" + fmt.Sprintf("%.0f", cfg.PeakMultiplier) + "）"
	if state == "offpeak" {
		desc = "空闲价（默认）"
	}
	return map[string]interface{}{
		"state":          state,
		"state_desc":     desc,
		"last_switch_at": nowUnix,
	}, nil
}

// MaybeApply switches pricing when the peak/off-peak state changed since the
// last application. Called by the background ticker every minute.
func (s *PeakPricingService) MaybeApply(now time.Time) error {
	cfg, err := s.GetConfig()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	state := "offpeak"
	if s.InPeakPeriod(cfg.PeakPeriods, now) {
		state = "peak"
	}
	if state == cfg.CurrentState {
		return nil
	}
	_, err = s.applyState(cfg, now)
	return err
}

// UpdateConfig validates and persists the settings; when enabling, captures
// UpdateConfig validates and persists the settings and immediately applies
// pricing: enabling saves/uses the off-peak baseline and applies the current
// period's price; adjusting periods/multiplier while enabled re-applies;
// disabling restores the default off-peak price.
// 默认价格 = 空闲价（基准）；高峰时段价格 = 空闲价 × PeakMultiplier。
// 开启/停用/调整后点保存立即生效（重写 new-api options）。
func (s *PeakPricingService) UpdateConfig(enabled bool, peakPeriods string, peakMultiplier float64) (map[string]interface{}, error) {
	if err := ValidatePeakPeriods(peakPeriods); err != nil {
		return nil, err
	}
	if peakMultiplier < 1 || peakMultiplier > 10 {
		return nil, fmt.Errorf("高峰价格倍数应在 1~10 之间（默认 2，表示高峰价 = 空闲价 × 2）")
	}

	db, err := s.openStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensurePeakPricingTables(ctx, db); err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	prevEnabled := false
	cur, err := s.GetConfig()
	if err == nil {
		prevEnabled = cur.Enabled
	}
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO peak_pricing_settings (id, enabled, peak_periods, peak_multiplier, current_state, last_switch_at, updated_at)
		VALUES (1, ?, ?, ?, 'offpeak', 0, ?)
		ON CONFLICT(id) DO UPDATE SET
			enabled = excluded.enabled,
			peak_periods = excluded.peak_periods,
			peak_multiplier = excluded.peak_multiplier,
			updated_at = excluded.updated_at`,
		enabledInt, peakPeriods, peakMultiplier, now)
	if err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"enabled":         enabled,
		"peak_periods":    peakPeriods,
		"peak_multiplier": peakMultiplier,
		"captured":        false,
	}

	if enabled {
		// 首次启用时捕获当前 new-api 定价作为空闲价（默认价）基准；后续启停复用，不覆盖
		if !prevEnabled {
			if _, err := s.CaptureBaseline(); err != nil {
				return nil, fmt.Errorf("捕获空闲价基准失败: %v", err)
			}
			result["captured"] = true
		}
		// 无论是否首次启用，都立即按当前时刻应用空闲价/高峰价，保证保存即生效
		cfg := PeakPricingConfig{Enabled: true, PeakPeriods: peakPeriods, PeakMultiplier: peakMultiplier, CurrentState: "offpeak"}
		if res, err := s.applyState(cfg, time.Now()); err != nil {
			return nil, err
		} else {
			result["state"] = res["state"]
			result["state_desc"] = res["state_desc"]
		}
	} else if prevEnabled {
		// 停用：恢复默认空闲价（基准）；未捕获过基准则跳过
		baseline, err := s.GetBaseline()
		if err != nil {
			return nil, err
		}
		if len(baseline.ModelRatio) > 0 {
			if err := s.applyStateFactor(1.0); err != nil {
				return nil, err
			}
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE peak_pricing_settings SET current_state = 'offpeak' WHERE id = 1`); err != nil {
			return nil, err
		}
		result["state_desc"] = "已恢复空闲价（默认）"
	}
	return result, nil
}

// QuotaPerMillion converts a new-api per-token ratio into 元/百万tokens:
// 1 元 = 500,000 quota, so 元/M = ratio × 1e6 / 500000 = ratio × 2.
const QuotaPerYuan = 500000

func ratioToYuanPerM(ratio float64) float64 {
	return ratio * 1e6 / QuotaPerYuan
}

// ModelPriceView is the model pricing (元/百万tokens) read directly from the
// captured off-peak baseline; peak price = off-peak × PeakMultiplier.
type ModelPriceView struct {
	Model         string  `json:"model"`
	PeakHit       float64 `json:"peak_hit"`    // 高峰 缓存命中 元/M
	PeakMiss      float64 `json:"peak_miss"`   // 高峰 输入未命中 元/M
	PeakOutput    float64 `json:"peak_output"` // 高峰 输出 元/M
	OffpeakHit    float64 `json:"offpeak_hit"`
	OffpeakMiss   float64 `json:"offpeak_miss"`
	OffpeakOutput float64 `json:"offpeak_output"`
}

// GetModelPricing reads the model pricing (元/百万tokens) directly from the
// off-peak baseline snapshot; empty baseline yields nil (UI shows "尚未捕获").
func (s *PeakPricingService) GetModelPricing(peakMultiplier float64) ([]ModelPriceView, error) {
	baseline, err := s.GetBaseline()
	if err != nil {
		return nil, err
	}
	if len(baseline.ModelRatio) == 0 {
		return nil, nil
	}
	if peakMultiplier < 1 || peakMultiplier > 10 {
		peakMultiplier = 2
	}
	views := make([]ModelPriceView, 0, len(DeepSeekV4Models))
	for _, model := range DeepSeekV4Models {
		mr, ok := baseline.ModelRatio[model]
		if !ok {
			continue
		}
		v := ModelPriceView{Model: model}
		cr := baseline.CompletionRatio[model]
		ccr := baseline.CacheRatio[model]
		// 基准 = 空闲价（默认）；高峰价 = 空闲价 × 倍数
		v.OffpeakHit = math.Round(ratioToYuanPerM(mr*ccr)*1e4) / 1e4
		v.OffpeakMiss = math.Round(ratioToYuanPerM(mr)*1e4) / 1e4
		v.OffpeakOutput = math.Round(ratioToYuanPerM(mr*cr)*1e4) / 1e4
		v.PeakHit = math.Round(v.OffpeakHit*peakMultiplier*1e4) / 1e4
		v.PeakMiss = math.Round(v.OffpeakMiss*peakMultiplier*1e4) / 1e4
		v.PeakOutput = math.Round(v.OffpeakOutput*peakMultiplier*1e4) / 1e4
		views = append(views, v)
	}
	return views, nil
}

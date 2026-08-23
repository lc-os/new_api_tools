package service

import (
	"testing"
	"time"
)

func bjTime(y int, m time.Month, d, h, min int) time.Time {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

func TestInPeakPeriod(t *testing.T) {
	svc := &PeakPricingService{}
	const periods = "09:00-12:00,14:00-18:00"

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		// 周一
		{"周一 09:00 高峰开始", bjTime(2026, 8, 24, 9, 0), true},
		{"周一 10:30 高峰", bjTime(2026, 8, 24, 10, 30), true},
		{"周一 11:59 高峰", bjTime(2026, 8, 24, 11, 59), true},
		{"周一 12:00 午休（非高峰）", bjTime(2026, 8, 24, 12, 0), false},
		{"周一 14:00 下午高峰开始", bjTime(2026, 8, 24, 14, 0), true},
		{"周一 17:59 下午高峰", bjTime(2026, 8, 24, 17, 59), true},
		{"周一 18:00 非高峰", bjTime(2026, 8, 24, 18, 0), false},
		{"周一 20:00 非高峰", bjTime(2026, 8, 24, 20, 0), false},
		// 周二-周五
		{"周五 09:00 高峰", bjTime(2026, 8, 28, 9, 0), true},
		{"周五 15:00 高峰", bjTime(2026, 8, 28, 15, 0), true},
		{"周五 18:00 非高峰", bjTime(2026, 8, 28, 18, 0), false},
		// 周末（即使落在高峰时间段内也是空闲）
		{"周六 10:00 非高峰", bjTime(2026, 8, 22, 10, 0), false},
		{"周六 15:00 非高峰", bjTime(2026, 8, 22, 15, 0), false},
		{"周日 10:00 非高峰", bjTime(2026, 8, 23, 10, 0), false},
		{"周日 09:00 非高峰", bjTime(2026, 8, 23, 9, 0), false},
	}

	for _, tc := range cases {
		if got := svc.InPeakPeriod(periods, tc.now); got != tc.want {
			t.Errorf("%s: InPeakPeriod(%s) = %v, want %v",
				tc.name, tc.now.Format("2006-01-02 15:04 Mon"), got, tc.want)
		}
	}
}

func TestDeepSeekV4ModelsIncludesVision(t *testing.T) {
	found := false
	for _, m := range DeepSeekV4Models {
		if m == "deepseek-v4-flash-vision-exp" {
			found = true
		}
	}
	if !found {
		t.Error("DeepSeekV4Models 应包含 deepseek-v4-flash-vision-exp")
	}
}

func TestApplyVisionFallback(t *testing.T) {
	b := PeakBaseline{
		ModelRatio:      map[string]float64{"deepseek-v4-flash": 0.75},
		CompletionRatio: map[string]float64{"deepseek-v4-flash": 3.0},
		CacheRatio:      map[string]float64{"deepseek-v4-flash": 0.03333333},
	}
	applyVisionFallback(&b)
	const v = "deepseek-v4-flash-vision-exp"
	if b.ModelRatio[v] != VisionModelFallbackModelRatio {
		t.Errorf("vision ModelRatio fallback = %v, want %v", b.ModelRatio[v], VisionModelFallbackModelRatio)
	}
	if b.CompletionRatio[v] != VisionModelFallbackCompletionRatio {
		t.Errorf("vision CompletionRatio fallback = %v, want %v", b.CompletionRatio[v], VisionModelFallbackCompletionRatio)
	}
	if b.CacheRatio[v] != VisionModelFallbackCacheRatio {
		t.Errorf("vision CacheRatio fallback = %v, want %v", b.CacheRatio[v], VisionModelFallbackCacheRatio)
	}
	// 幂等：已存在时不覆盖
	b.ModelRatio[v] = 0.99
	applyVisionFallback(&b)
	if b.ModelRatio[v] != 0.99 {
		t.Error("applyVisionFallback 不应覆盖已存在的值")
	}
}

// TestVisionPriceRoundTrip verifies the vision-exp fallback converts to the
// official 元/百万tokens values shown in the UI (空闲 1.5/0.05/4.5, 高峰 ×2).
func TestVisionPriceRoundTrip(t *testing.T) {
	b := PeakBaseline{
		ModelRatio:      map[string]float64{"deepseek-v4-flash-vision-exp": VisionModelFallbackModelRatio},
		CompletionRatio: map[string]float64{"deepseek-v4-flash-vision-exp": VisionModelFallbackCompletionRatio},
		CacheRatio:      map[string]float64{"deepseek-v4-flash-vision-exp": VisionModelFallbackCacheRatio},
	}
	svc := &PeakPricingService{}
	views, err := svc.getModelPricingFrom(b, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views len = %d, want 1", len(views))
	}
	v := views[0]
	if v.OffpeakMiss != 1.5 {
		t.Errorf("空闲 miss = %v, want 1.5", v.OffpeakMiss)
	}
	if v.OffpeakHit != 0.05 {
		t.Errorf("空闲 hit = %v, want 0.05", v.OffpeakHit)
	}
	if v.OffpeakOutput != 4.5 {
		t.Errorf("空闲 output = %v, want 4.5", v.OffpeakOutput)
	}
	if v.PeakMiss != 3.0 {
		t.Errorf("高峰 miss = %v, want 3.0", v.PeakMiss)
	}
	if v.PeakHit != 0.1 {
		t.Errorf("高峰 hit = %v, want 0.1", v.PeakHit)
	}
	if v.PeakOutput != 9.0 {
		t.Errorf("高峰 output = %v, want 9.0", v.PeakOutput)
	}
}

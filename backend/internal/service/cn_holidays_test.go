package service

import (
	"testing"
	"time"
)

func mustDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func TestIsSkippedRefreshDay(t *testing.T) {
	cases := []struct {
		name          string
		date          time.Time
		skipWeekends  bool
		skipHolidays  bool
		wantSkipped   bool
	}{
		// 普通工作日（不跳过任何）
		{"2025-03-05 周三 普通工作日", mustDate(2025, 3, 5), true, true, false},
		// 普通周六（仅跳过周末）
		{"2025-03-08 周六 普通周末", mustDate(2025, 3, 8), true, false, true},
		{"2025-03-08 周六 不跳过周末", mustDate(2025, 3, 8), false, false, false},
		// 普通周日（仅跳过节假日时不跳过）
		{"2025-03-09 周日 仅跳过节假日", mustDate(2025, 3, 9), false, true, false},
		// 2025 春节（工作日放假 1.28-2.4）
		{"2025-02-03 春节 工作日放假", mustDate(2025, 2, 3), false, true, true},
		{"2025-02-03 春节 不跳过节假日", mustDate(2025, 2, 3), false, false, false},
		// 春节连休覆盖的周末（2.1-2.2 周六日）：跳过节假日时算节假日跳过
		{"2025-02-01 春节连休周六", mustDate(2025, 2, 1), false, true, true},
		// 2025 调休上班日（1.26 周日、2.8 周六）：永不跳过
		{"2025-01-26 周日调休上班", mustDate(2025, 1, 26), true, true, false},
		{"2025-02-08 周六调休上班", mustDate(2025, 2, 8), true, true, false},
		{"2025-10-11 周六国庆调休", mustDate(2025, 10, 11), true, true, false},
		// 2025 劳动节 5.1-5.5
		{"2025-05-04 劳动节连休周日", mustDate(2025, 5, 4), false, true, true},
		// 2025 国庆+中秋 10.1-10.8
		{"2025-10-06 国庆", mustDate(2025, 10, 6), false, true, true},
		// 2026 春节 2.15-2.23
		{"2026-02-19 春节 工作日放假", mustDate(2026, 2, 19), false, true, true},
		{"2026-02-16 春节连休周一", mustDate(2026, 2, 16), true, true, true},
		// 2026 调休上班日
		{"2026-02-14 周六春节调休", mustDate(2026, 2, 14), true, true, false},
		{"2026-02-28 周六春节调休", mustDate(2026, 2, 28), true, true, false},
		{"2026-01-04 周日元旦调休", mustDate(2026, 1, 4), true, true, false},
		{"2026-05-09 周六劳动节调休", mustDate(2026, 5, 9), true, true, false},
		{"2026-09-20 周日国庆调休", mustDate(2026, 9, 20), true, true, false},
		{"2026-10-10 周六国庆调休", mustDate(2026, 10, 10), true, true, false},
		// 2026 元旦 1.1-1.3
		{"2026-01-02 元旦", mustDate(2026, 1, 2), false, true, true},
		// 2026 端午 6.19-6.21
		{"2026-06-20 端午周六", mustDate(2026, 6, 20), false, true, true},
		{"2026-06-20 端午周六 仅跳过周末", mustDate(2026, 6, 20), true, false, true},
		// 2026 中秋 9.25-9.27
		{"2026-09-26 中秋周六", mustDate(2026, 9, 26), false, true, true},
		// 2026 国庆 10.1-10.7
		{"2026-10-05 国庆", mustDate(2026, 10, 5), false, true, true},
		// 节假日与周末同时跳过：春节连休的周六周日
		{"2026-02-21 春节连休周六", mustDate(2026, 2, 21), true, true, true},
		// 法定节假日后的普通周六（10.17）：仅跳过节假日时不跳过
		{"2026-10-17 普通周六 仅跳过节假日", mustDate(2026, 10, 17), false, true, false},
	}

	for _, tc := range cases {
		got := IsSkippedRefreshDay(tc.date, tc.skipWeekends, tc.skipHolidays, nil)
		if got != tc.wantSkipped {
			t.Errorf("%s: IsSkippedRefreshDay(%s, skipWeekends=%v, skipHolidays=%v) = %v, want %v",
				tc.name, tc.date.Format("2006-01-02")+weekdayCN(tc.date), tc.skipWeekends, tc.skipHolidays, got, tc.wantSkipped)
		}
	}
}

func weekdayCN(t time.Time) string {
	return []string{"日", "一", "二", "三", "四", "五", "六"}[int(t.Weekday())]
}

func TestIsCNHolidayWorkday(t *testing.T) {
	if !IsCNHoliday(mustDate(2026, 2, 17)) {
		t.Error("2026-02-17 应为春节假期")
	}
	if IsCNHoliday(mustDate(2026, 2, 14)) {
		t.Error("2026-02-14 是调休上班日，不应算节假日")
	}
	if IsCNWorkday(mustDate(2026, 2, 14)) != true {
		t.Error("2026-02-14 应为调休上班日")
	}
	if IsCNWorkday(mustDate(2026, 2, 15)) {
		t.Error("2026-02-15 春节假期首日，不是调休上班日")
	}
	if !IsCNHoliday(mustDate(2025, 10, 1)) {
		t.Error("2025-10-01 应为国庆假期")
	}
}

func TestIsSkippedRefreshDayCustomOffDays(t *testing.T) {
	off := map[string]bool{"2026-03-06": true} // 公司自定义休息日（周五）
	// 自定义休息日：即使工作日、不跳过任何规则也跳过
	if !IsSkippedRefreshDay(mustDate(2026, 3, 6), false, false, off) {
		t.Error("自定义休息日（周五）应被跳过")
	}
	// 非自定义休息日的普通工作日不受影响
	if IsSkippedRefreshDay(mustDate(2026, 3, 5), false, false, off) {
		t.Error("普通工作日不应被跳过")
	}
	// 自定义休息日覆盖的周末：周末 + 自定义休息日均跳过（连休场景）
	offWeekend := map[string]bool{"2026-03-06": true, "2026-03-07": true} // 周五+周六连休
	if !IsSkippedRefreshDay(mustDate(2026, 3, 7), false, false, offWeekend) {
		t.Error("自定义休息日覆盖的周六应被跳过")
	}
	// 自定义休息日优先级最高：即使当天是调休上班日也跳过（管理员手动指定优先）
	if !IsSkippedRefreshDay(mustDate(2026, 2, 14), false, false, map[string]bool{"2026-02-14": true}) {
		t.Error("自定义休息日应优先于调休上班日")
	}
}

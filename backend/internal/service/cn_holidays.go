package service

import "time"

// 中国法定节假日（国务院办公厅每年发布的放假安排）：
//   - cnHolidayDates: 放假日期（含调休连休；含落在周末的日期，因为连休覆盖周末）
//   - cnWorkdayDates: 调休上班日（通常为周末，按工作日处理）
//
// 数据来源：
//   - 2025: 国务院办公厅 2024-11-12 发布（春节 1.28-2.4 调休 8 天；劳动节 5.1-5.5；国庆中秋 10.1-10.8）
//   - 2026: 国务院办公厅 2025-11-04 发布（元旦 1.1-1.3；春节 2.15-2.23 调休 9 天；劳动节 5.1-5.5；
//     端午 6.19-6.21；中秋 9.25-9.27；国庆 10.1-10.7）
//
// 每年国务院发布新安排后，在此追加对应年份的日期即可（格式 YYYY-MM-DD）。

var cnHolidayDates = map[string]bool{
	// ===== 2025 =====
	// 元旦：1月1日放假1天，不调休
	"2025-01-01": true,
	// 春节：1月28日（除夕）至2月4日放假调休，共8天
	"2025-01-28": true, "2025-01-29": true, "2025-01-30": true, "2025-01-31": true,
	"2025-02-01": true, "2025-02-02": true, "2025-02-03": true, "2025-02-04": true,
	// 清明节：4月4日至6日放假，共3天
	"2025-04-04": true, "2025-04-05": true, "2025-04-06": true,
	// 劳动节：5月1日至5日放假调休，共5天
	"2025-05-01": true, "2025-05-02": true, "2025-05-03": true, "2025-05-04": true, "2025-05-05": true,
	// 端午节：5月31日至6月2日放假，共3天
	"2025-05-31": true, "2025-06-01": true, "2025-06-02": true,
	// 国庆节、中秋节：10月1日至8日放假调休，共8天
	"2025-10-01": true, "2025-10-02": true, "2025-10-03": true, "2025-10-04": true,
	"2025-10-05": true, "2025-10-06": true, "2025-10-07": true, "2025-10-08": true,

	// ===== 2026 =====
	// 元旦：1月1日至3日放假调休，共3天
	"2026-01-01": true, "2026-01-02": true, "2026-01-03": true,
	// 春节：2月15日至23日放假调休，共9天
	"2026-02-15": true, "2026-02-16": true, "2026-02-17": true, "2026-02-18": true,
	"2026-02-19": true, "2026-02-20": true, "2026-02-21": true, "2026-02-22": true, "2026-02-23": true,
	// 清明节：4月4日至6日放假，共3天
	"2026-04-04": true, "2026-04-05": true, "2026-04-06": true,
	// 劳动节：5月1日至5日放假调休，共5天
	"2026-05-01": true, "2026-05-02": true, "2026-05-03": true, "2026-05-04": true, "2026-05-05": true,
	// 端午节：6月19日至21日放假，共3天
	"2026-06-19": true, "2026-06-20": true, "2026-06-21": true,
	// 中秋节：9月25日至27日放假，共3天
	"2026-09-25": true, "2026-09-26": true, "2026-09-27": true,
	// 国庆节：10月1日至7日放假调休，共7天
	"2026-10-01": true, "2026-10-02": true, "2026-10-03": true, "2026-10-04": true,
	"2026-10-05": true, "2026-10-06": true, "2026-10-07": true,
}

// cnWorkdayDates: 调休上班日（周末补班，按工作日处理，不跳过）
var cnWorkdayDates = map[string]bool{
	// ===== 2025 调休上班 =====
	"2025-01-26": true, // 春节调休（周日）
	"2025-02-08": true, // 春节调休（周六）
	"2025-04-27": true, // 劳动节调休（周日）
	"2025-09-28": true, // 国庆调休（周日）
	"2025-10-11": true, // 国庆调休（周六）

	// ===== 2026 调休上班 =====
	"2026-01-04": true, // 元旦调休（周日）
	"2026-02-14": true, // 春节调休（周六）
	"2026-02-28": true, // 春节调休（周六）
	"2026-05-09": true, // 劳动节调休（周六）
	"2026-09-20": true, // 国庆调休（周日）
	"2026-10-10": true, // 国庆调休（周六）
}

// IsCNHoliday reports whether t falls on a statutory holiday (or its makeup
// break). 调休上班日不算节假日；普通周末也不在此列。
func IsCNHoliday(t time.Time) bool {
	return cnHolidayDates[t.Format("2006-01-02")]
}

// IsCNWorkday reports whether t is a weekend that is a makeup workday
// (调休上班日), i.e. a Saturday/Sunday that must be treated as a workday.
func IsCNWorkday(t time.Time) bool {
	return cnWorkdayDates[t.Format("2006-01-02")]
}

// IsSkippedRefreshDay decides whether a scheduled refresh on day t should be
// skipped given the scheduling rules:
//   - makeup workdays (调休上班) are never skipped;
//   - weekends (Sat/Sun, excluding makeup workdays) are skipped when skipWeekends;
//   - statutory holidays are skipped when skipHolidays;
//   - extra custom off days (公司自定义休息日) are always skipped when present
//     in offDays.
//
// A holiday that falls on a weekend (covered by 连休) is already in
// cnHolidayDates, so it is skipped when skipHolidays is on — but a plain
// weekend with skipWeekends off and skipHolidays on is NOT skipped (它只是普通
// 周末，不属于法定节假日)，与“只跳过法定节假日”的语义一致。
func IsSkippedRefreshDay(t time.Time, skipWeekends, skipHolidays bool, offDays map[string]bool) bool {
	dateStr := t.Format("2006-01-02")
	if offDays[dateStr] {
		return true // 自定义休息日：始终跳过
	}
	if IsCNWorkday(t) {
		return false // 调休上班日：按工作日处理，永不跳过
	}
	wd := t.Weekday()
	isWeekend := wd == time.Saturday || wd == time.Sunday
	if skipWeekends && isWeekend {
		return true
	}
	if skipHolidays && cnHolidayDates[dateStr] {
		return true
	}
	return false
}

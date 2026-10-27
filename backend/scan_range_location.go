package main

// A failed multi-sector request does not prove every sector in it failed.
// Use recorded probe evidence, never the requested task settings, to describe
// the precision. Missing or inconsistent old evidence remains unknown.
func scanRangePrecision(r BadRange, sector int64) string {
	if r.Outcome == "unresolved" || r.Code == "LOCALIZATION_LIMIT" || r.Code == "SLOW_READ_LIMIT" {
		return "unresolved"
	}
	if r.Outcome == "" {
		return "unknown"
	}
	if r.Outcome != "unreadable" {
		return "request"
	}
	if sector <= 0 || r.Offset < 0 || r.Length <= 0 || r.Offset%sector != 0 || r.Length%sector != 0 ||
		r.ProbeBytes < r.Length || r.ProbeBytes%sector != 0 || r.Recovered == nil || *r.Recovered || deviceScanIssue(r) {
		return "unknown"
	}
	if r.ProbeBytes == sector && r.Length == sector {
		return "sector"
	}
	return "range"
}

func scanRangePrecisionLabel(r BadRange, sector int64) string {
	return map[string]string{
		"sector":     "逐扇区确认",
		"range":      "范围确认（未逐扇区）",
		"unresolved": "待定位（未确认坏扇区）",
		"unknown":    "定位精度未记录",
		"request":    "请求范围",
	}[scanRangePrecision(r, sector)]
}

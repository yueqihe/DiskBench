package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

//go:embed scan_report.html
var scanReportHTML string

//go:embed scan_report.css
var scanReportCSS string

const scanReportPrintJS = `document.getElementById('print-report').addEventListener('click',function(){window.print();});`

func scanReportCSP() string {
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return "default-src 'none'; base-uri 'none'; form-action 'none'; style-src " + hash(scanReportCSS) + "; script-src " + hash(scanReportPrintJS)
}

var scanHTMLTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"addInt":       func(a, b int64) int64 { return a + b },
	"reportStyle":  func() template.CSS { return template.CSS(scanReportCSS) },
	"reportScript": func() template.JS { return template.JS(scanReportPrintJS) },
}).Parse(scanReportHTML))

type reportField struct{ Label, Value string }
type reportMetric struct{ Label, Value, Note string }
type reportBand struct {
	Key, Label string
	Count      int
	X, Width   float64
}
type reportSlowRow struct{ Region, Address, Latency, Outcome string }
type reportSmartRow struct {
	Label, Before, After, Change string
	Increased                    bool
}
type reportIssueRow struct{ Address, Size, Precision, Probe, Outcome, Before, BeforeRepair, After, AfterLabel, Detail string }

type scanHTMLView struct {
	Job                        *Job
	Generated, Mode, Status    string
	Verdict, Explanation, Tone string
	CSP                        string
	Identity, Config           []reportField
	Metrics                    []reportMetric
	Bands                      []reportBand
	SlowRows                   []reportSlowRow
	SlowCount                  int
	SmartRows                  []reportSmartRow
	SmartNotes                 []string
	Repair                     *ScanRepairStats
	RepairThreshold            int64
	Mock, Legacy, Complete     bool
	IssueRows, RawRecords      int64
	Warning                    string
}

func reportText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未记录"
	}
	return s
}

func reportBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value, unit := float64(n), "B"
	for _, u := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= 1000
		unit = u
		if value < 1000 {
			break
		}
	}
	return fmt.Sprintf("%.2f %s", value, unit)
}

func reportBinaryBytes(n int64) string {
	value, unit := float64(n), "B"
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		if value < 1024 {
			break
		}
		value /= 1024
		unit = u
	}
	return fmt.Sprintf("%.2f %s", value, unit)
}

func reportTime(value *string) string {
	if value == nil || *value == "" {
		return "未记录"
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return *value
	}
	return parsed.In(time.Local).Format("2006-01-02 15:04:05 -07:00")
}

func reportElapsed(job *Job, now time.Time) string {
	if job.StartedAt == nil {
		return "未记录"
	}
	start, err := time.Parse(time.RFC3339Nano, *job.StartedAt)
	if err != nil {
		return "未记录"
	}
	end := now
	if job.FinishedAt != nil {
		end, err = time.Parse(time.RFC3339Nano, *job.FinishedAt)
		if err != nil {
			return "未记录"
		}
	} else if job.Status != "running" && job.Status != "cancelling" {
		return "未记录结束时间"
	}
	if end.Before(start) {
		return "时间记录不一致"
	}
	seconds := int64(end.Sub(start).Seconds())
	value := fmt.Sprintf("%d 小时 %d 分 %d 秒", seconds/3600, seconds/60%60, seconds%60)
	if job.FinishedAt == nil {
		value += "（截至导出）"
	}
	return value
}

func reportAddress(offset, length, sector int64) string {
	if sector > 0 {
		return fmt.Sprintf("LBA %d–%d", offset/sector, (offset+length-1)/sector)
	}
	return fmt.Sprintf("字节 %d–%d", offset, offset+length-1)
}

func reportCellGroup(cell ScanCell, latencyBasis string) string {
	if cell.Status == "normal" && cell.MaxUnitMs != nil {
		if *cell.MaxUnitMs < 5 {
			return "fast"
		}
		if *cell.MaxUnitMs < 20 {
			return "good"
		}
	}
	poorAfterMS := 150.0
	if strings.HasSuffix(latencyBasis, "-dual-v4") {
		_, poorAfterMS, _ = scanReadThresholds(strings.HasPrefix(latencyBasis, "hdd-"))
	}
	if cell.Status == "slow" && (cell.MaxUnitMs == nil || *cell.MaxUnitMs >= poorAfterMS) {
		return "slower"
	}
	if group := map[string]string{"under5": "fast", "under20": "good", "under50": "normal", "under150": "slow", "under500": "slower", "over500": "severe", "error": "severe", "reading": "pending"}[cell.Status]; group != "" {
		return group
	}
	return cell.Status
}

func reportRepairMilliseconds(ms float64) string {
	if ms < 1 {
		return fmt.Sprintf("%.2f ms", ms)
	}
	return fmt.Sprintf("%.1f ms", ms)
}

func reportIssue(r BadRange, sector int64, repairThreshold float64) reportIssueRow {
	row := reportIssueRow{Address: reportAddress(r.Offset, r.Length, sector), Size: reportBinaryBytes(r.Length), Precision: scanRangePrecisionLabel(r, sector), Before: "—", After: "—", Detail: r.Code}
	if sector > 0 {
		row.Size += fmt.Sprintf(" · 逻辑扇区 %d B", sector)
	}
	if r.ProbeBytes > 0 && (scanRangePrecision(r, sector) == "sector" || scanRangePrecision(r, sector) == "range") {
		row.Probe = fmt.Sprintf("实际定位单位 %d B", r.ProbeBytes)
	}
	if r.SkippedRange != nil {
		row.Outcome = "读取超时，已跳过"
		row.Precision = "请求范围 · 跳过部分待定位"
		row.Detail = "读取超时；跳过 " + reportAddress(r.SkippedRange.Offset, r.SkippedRange.Length, sector) + "（" + reportBinaryBytes(r.SkippedRange.Length) + "），未完成读取检测"
		return row
	}
	if r.Code == "SLOW_READ_LIMIT" {
		row.Outcome, row.Detail = "严重卡读，已跳过", r.Detail
		return row
	}
	row.Outcome = map[string]string{"severe": "严重慢读（待确认）", "unreadable": "坏块（不可读）", "recovered": "复读恢复 / 固件纠正", "device_error": "设备或连接异常", "unresolved": "异常范围待细查"}[r.Outcome]
	if row.Outcome == "" {
		row.Outcome = "历史异常记录"
	}
	if r.Outcome == "severe" {
		if r.Code == "PERSISTENT_SLOW_READ" {
			row.Outcome = "坏块（慢速）"
		}
		row.Detail = r.Detail
		return row
	}
	if r.Outcome == "rewrite" && r.Repair != nil {
		repair := r.Repair
		row.Outcome = map[string]string{"pending": "已备份，等待最终结果", "uncertain": "写回中断，结果未确定", "verified": "写回校验通过", "still_slow": "修复后仍慢", "skipped": "已跳过写回", "failed": "写回或校验失败"}[repair.Outcome]
		if repair.Outcome == "verified" && repair.SpeedOutcome == "improved" {
			row.Outcome = "慢速改善 · 写回校验通过"
		}
		row.Before = fmt.Sprintf("%.1f ms", repair.BeforeMs)
		if repair.BeforeRepeatMs != nil {
			row.BeforeRepair = reportRepairMilliseconds(*repair.BeforeRepeatMs)
		}
		if repair.AfterMs != nil {
			row.After = fmt.Sprintf("%.1f ms", *repair.AfterMs)
			row.AfterLabel = "校验"
		}
		row.Detail = repair.Detail
		if repair.Outcome == "verified" || repair.Outcome == "still_slow" {
			if row.Detail == "" || strings.HasPrefix(row.Detail, "写回校验通过；") {
				row.Detail = repairSpeedNote(repair, repairThreshold)
			}
			if len(repair.SpeedChecks) == 2 {
				row.AfterLabel = "复测"
				row.After = strings.TrimSuffix(reportRepairMilliseconds(repair.SpeedChecks[0]), " ms") + " / " + reportRepairMilliseconds(repair.SpeedChecks[1])
			} else if len(repair.SpeedChecks) == 1 {
				row.Detail = "复测未完成"
			}
		} else if row.Detail == "" && repair.Outcome == "pending" {
			row.Detail = "已备份，等待写回"
		}
		return row
	}
	labels := map[string]string{"EIO": "I/O 读取失败，设备未细分原因", "MEDIUM_ERROR": "设备报告介质读取错误", "RECOVERED_ERROR": "固件纠正了读取错误", "READ_RETRY": "首次读取异常，已进行复读", "SHORT_READ": "未完整读出请求数据", "TIMEOUT": "读取超时", "TRANSPORT_ERROR": "连接或传输错误", "CONTROLLER_ERROR": "控制器错误", "HARDWARE_ERROR": "设备硬件错误", "NOT_READY": "设备未就绪", "UNIT_ATTENTION": "设备状态发生变化", "COMMAND_ABORTED": "读取命令被中止", "COMMAND_ERROR": "读取命令失败", "ILLEGAL_REQUEST": "设备拒绝读取命令", "DATA_INTEGRITY_ERROR": "设备报告数据完整性错误", "LOCALIZATION_LIMIT": "已达到异常定位预算，剩余范围待细查", "ACCESS_DENIED": "设备拒绝访问", "UNWRITTEN_LBA": "未分配或未写入的地址"}
	if label := labels[r.Code]; label != "" {
		row.Detail = label + "（" + r.Code + "）"
	}
	if r.UnresolvedBytes > 0 {
		row.Detail += "；尚待细查 " + reportBytes(r.UnresolvedBytes)
	}
	if r.Evidence != nil && r.Evidence.LBA != nil {
		row.Detail += fmt.Sprintf("；设备日志地址线索 LBA %d", *r.Evidence.LBA)
	}
	return row
}

// Read a fixed snapshot. A damaged journal still produces a readable partial
// report with a prominent completeness warning; writer errors abort the stream.
func walkScanReportIssues(job *Job, file *os.File, size int64, visit func(BadRange) error) (int64, string, error) {
	r := job.Result.(*ScanResult)
	warning := r.Stats.IssueWarning
	count := int64(0)
	if r.Version < 2 {
		warning = "旧记录仅保留当时的摘要，无法确认异常地址是否完整。"
		for _, record := range r.Stats.BadRanges {
			if err := visit(record); err != nil {
				return count, warning, err
			}
			count++
		}
		return count, warning, nil
	}
	if file == nil {
		return count, warning, nil
	}
	scanner := issueScanner(io.NewSectionReader(file, 0, size))
	for count < r.Stats.SavedIssueCount && scanner.Scan() {
		var record BadRange
		if json.Unmarshal(scanner.Bytes(), &record) != nil || !validScanJournalRecord(record, job.Disk.Size) {
			return count, "异常记录损坏，本报告仅包含可读取的部分，不能据此判断全部异常。", nil
		}
		if err := visit(record); err != nil {
			return count, warning, err
		}
		count++
	}
	if scanner.Err() != nil || count < r.Stats.SavedIssueCount {
		warning = "异常记录被截断或无法完整读取，本报告的地址明细不完整。"
	}
	return count, warning, nil
}

func (m *JobManager) prepareScanHTMLReport(id string) (func(io.Writer) error, string, error) {
	job, file, size, err := m.scanIssueSnapshot(id)
	if err != nil {
		return nil, "", err
	}
	generated := time.Now()
	view := makeScanHTMLView(job, generated)
	result := job.Result.(*ScanResult)
	severeRanges := scanSevereRanges(result, job.Disk.Rotational)
	rows := scanIssueRows{emit: func(BadRange) error { view.IssueRows++; return nil }}
	view.RawRecords, view.Warning, err = walkScanReportIssues(job, file, size, rows.add)
	if err == nil {
		err = rows.flush()
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		return nil, "", err
	}
	view.IssueRows += int64(len(severeRanges))
	view.Complete = !view.Legacy && result.Stats.IssuesComplete && view.Warning == "" && view.RawRecords == result.Stats.IssueCount
	if !view.Complete && view.Warning == "" {
		view.Warning = "部分已发现的异常未能保存完整地址，请结合扫描告警判断。"
	}
	if !view.Complete && view.Tone == "ok" {
		view.Tone, view.Verdict = "warning", "记录不完整，无法给出完整检测结论"
	}
	name := strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, reportText(job.Disk.Model))
	nameRunes := []rune(name)
	if len(nameRunes) > 80 {
		name = string(nameRunes[:80])
	}
	stamp := generated
	if job.StartedAt != nil {
		if start, err := time.Parse(time.RFC3339Nano, *job.StartedAt); err == nil {
			stamp = start
		}
	}
	filename := "DiskBench_" + name + "_" + view.Mode + "_" + stamp.In(time.Local).Format("20060102_150405") + ".html"
	return func(w io.Writer) error {
		if file != nil {
			defer file.Close()
		}
		if err := scanHTMLTemplate.ExecuteTemplate(w, "header", view); err != nil {
			return err
		}
		written := int64(0)
		rows := scanIssueRows{emit: func(r BadRange) error {
			written++
			_, _, severe := scanReadThresholds(job.Disk.Rotational)
			return scanHTMLTemplate.ExecuteTemplate(w, "issue", reportIssue(r, job.Disk.LogicalSectorSize, math.Min(scanRepairThreshold(job), severe)))
		}}
		_, warning, err := walkScanReportIssues(job, file, size, rows.add)
		if err != nil {
			return err
		}
		if err := rows.flush(); err != nil {
			return err
		}
		for _, r := range severeRanges {
			if err := rows.emit(r); err != nil {
				return err
			}
		}
		if warning != "" {
			view.Complete, view.Warning = false, warning
		}
		if written != view.IssueRows {
			view.Complete, view.Warning = false, "导出期间异常记录发生变化，明细可能不完整。"
		}
		view.IssueRows = written
		return scanHTMLTemplate.ExecuteTemplate(w, "footer", view)
	}, filename, nil
}

func makeScanHTMLView(job *Job, now time.Time) scanHTMLView {
	r := job.Result.(*ScanResult)
	s := r.Stats
	v := scanHTMLView{Job: job, Generated: now.In(time.Local).Format("2006-01-02 15:04:05 -07:00"), CSP: scanReportCSP(), Legacy: r.Version < 2,
		Status: map[string]string{"queued": "等待开始", "running": "扫描中", "cancelling": "正在停止", "completed": "已完成", "cancelled": "已手动停止", "interrupted": "意外中断", "failed": "执行失败"}[job.Status]}
	v.Mode = "全盘扫描"
	method := "连续读取整盘地址范围"
	if r.Coverage == "sampled" || r.ScanMode == "quick" {
		v.Mode, method = "快速抽样扫描", "按区域抽读部分数据"
		if r.SampleBytes != nil {
			method += "，每格 " + reportBinaryBytes(*r.SampleBytes)
		}
		if r.SamplingPlan == stratifiedScanPlan {
			method += "，格内分散取样"
		}
	}
	if job.Options.ScanRepairFrom != "" {
		v.Mode, method = "按记录修复慢区", "仅重新读取、备份并原样写回历史记录中的准确慢读地址，再读回校验；未重新扫描整盘"
	}
	diskType := "固态 / 非机械磁盘"
	if job.Disk.Rotational {
		diskType = "机械硬盘"
	} else if strings.HasPrefix(job.Disk.Name, "nvme") {
		diskType = "NVMe 固态硬盘"
	}
	capacity := "未记录"
	if job.Disk.Size > 0 {
		capacity = fmt.Sprintf("%s / %s（%d 字节）", reportBytes(job.Disk.Size), reportBinaryBytes(job.Disk.Size), job.Disk.Size)
	}
	sector := func(n int64) string {
		if n <= 0 {
			return "未记录"
		}
		return fmt.Sprintf("%d B", n)
	}
	v.Identity = []reportField{{"硬盘型号", reportText(job.Disk.Model)}, {"标称容量 / 二进制容量", capacity}, {"序列号", reportText(job.Disk.Serial)}, {"设备路径", reportText(job.Device)}, {"设备类型", diskType}, {"逻辑 / 物理扇区", sector(job.Disk.LogicalSectorSize) + " / " + sector(job.Disk.PhysicalSectorSize)}}
	finished := reportTime(job.FinishedAt)
	if job.FinishedAt == nil && (job.Status == "running" || job.Status == "queued" || job.Status == "cancelling") {
		finished = "尚未结束"
	}
	locate := "按扫描时的默认参数"
	if job.Options.ScanExactSectors != nil && !*job.Options.ScanExactSectors {
		locate = "64 KiB 区段定位（未逐扇区）"
	} else if job.Options.ScanDeepLocate != nil && *job.Options.ScanDeepLocate {
		locate = "精确定位 · 深入预算"
	} else if job.Options.ScanExactSectors != nil {
		locate = "精确定位 · 标准预算"
	}
	v.Config = []reportField{{"检测方式", method}, {"任务状态", v.Status}, {"开始时间", reportTime(job.StartedAt)}, {"结束时间", finished}, {"检测用时", reportElapsed(job, now)}, {"异常定位", locate}}
	if job.Options.ScanRepairFrom != "" {
		v.Config[len(v.Config)-1] = reportField{"来源扫描记录", job.Options.ScanRepairFrom}
		v.Config = append(v.Config, reportField{"选取范围", "每个原扫描区域保存的最慢单次读取请求，未包含未记录的其他地址"})
	}
	if strings.HasSuffix(r.LatencyBasis, "-dual-v4") {
		slow, poor, severe := scanReadThresholds(strings.HasPrefix(r.LatencyBasis, "hdd-"))
		v.Config = append(v.Config, reportField{"读取延迟分档", fmt.Sprintf("一般 %.0f–<%.0f ms；较差 %.0f–<%.0f ms；严重 ≥%.0f ms（红色）；单次读取与传输折算分别判断", slow, poor, poor, severe, severe)})
	}
	if job.Message != "" {
		message := job.Message
		if job.Status == "completed" && (len(scanSevereRanges(r)) > 0 || s.SkippedBytes > 0) {
			message = scanOutcomeCompletionMessage(v.Mode, r, s.ErrorCells, s.SlowCells, job.Disk.Rotational)
		}
		v.Config = append(v.Config, reportField{"任务说明", message})
	}
	if job.ResumeFrom != "" {
		v.Config = append(v.Config, reportField{"断点续扫", fmt.Sprintf("续接任务 %s，从 %.2f%% 处继续；结果包含此前已保存的区域", job.ResumeFrom, job.ResumeStartProgress)})
	}
	progress := job.Progress
	if math.IsNaN(progress) || math.IsInf(progress, 0) {
		progress = 0
	}
	v.Metrics = []reportMetric{{"任务进度", fmt.Sprintf("%.2f%%", max(0, min(100, progress))), fmt.Sprintf("已处理 %d / %d 个区域", s.TestedCells, len(r.Map))}, {"累计读取量", reportBytes(s.BytesRead), "可能包含复读，不等同于整盘覆盖率"}, {"持续不可读", fmt.Sprintf("%d 个范围", s.UnreadableRanges), reportBytes(s.UnreadableBytes)}, {"异常区域", fmt.Sprintf("%d 个", scanAbnormalCellCount(r)), "包含严重慢读与读取失败"}}
	bad := scanBadBlocks(r, job.Disk.Rotational)
	v.Metrics[3] = reportMetric{"坏块区域", fmt.Sprintf("%d 个", bad.Total), fmt.Sprintf("慢速 %d · 不可读 %d；按区域去重", bad.Slow, bad.Unreadable)}
	if job.Options.ScanRepairFrom != "" {
		v.Metrics[1] = reportMetric{"定位读取量", reportBytes(s.BytesRead), "不包含后续稳定性复读及写回校验"}
	}
	if s.SkippedBytes > 0 {
		v.Config = append(v.Config, reportField{"卡读跳过", reportBytes(s.SkippedBytes) + "，跳过范围未完成读取检测，不计入读取覆盖率"})
	}
	if v.Legacy {
		v.Metrics[2] = reportMetric{"读取异常区域", fmt.Sprintf("%d 个", s.ErrorCells), "旧记录未区分持续不可读与恢复告警"}
	} else if job.Disk.Size > 0 {
		v.Config = append(v.Config, reportField{"读取判定覆盖整盘", fmt.Sprintf("%.3f%%（%s）", float64(s.CoveredBytes)*100/float64(job.Disk.Size), reportBytes(s.CoveredBytes))})
	}
	counts := map[string]int{}
	slowCells := []ScanCell{}
	for _, cell := range r.Map {
		group := reportCellGroup(cell, r.LatencyBasis)
		counts[group]++
		if group == "slow" || group == "slower" || group == "severe" {
			slowCells = append(slowCells, cell)
		}
	}
	x := 0.0
	for _, band := range []reportBand{{Key: "fast", Label: "优秀"}, {Key: "good", Label: "良好"}, {Key: "normal", Label: "正常"}, {Key: "slow", Label: "一般"}, {Key: "slower", Label: "较差"}, {Key: "severe", Label: "严重 / 读取异常"}, {Key: "pending", Label: "未完成"}} {
		band.Count, band.X = counts[band.Key], x
		if len(r.Map) > 0 {
			band.Width = float64(band.Count) * 1000 / float64(len(r.Map))
		}
		x += band.Width
		v.Bands = append(v.Bands, band)
	}
	latency := func(cell ScanCell) float64 {
		if cell.MaxReadLatency != nil {
			return cell.MaxReadLatency.Ms
		}
		if cell.MaxMs != nil {
			return *cell.MaxMs
		}
		return 0
	}
	sort.SliceStable(slowCells, func(i, j int) bool { return latency(slowCells[i]) > latency(slowCells[j]) })
	v.SlowCount = len(slowCells)
	for _, cell := range slowCells[:min(20, len(slowCells))] {
		outcome := map[string]string{"slow": "一般", "slower": "较差", "severe": "严重 / 读取异常"}[reportCellGroup(cell, r.LatencyBasis)]
		if cell.Recheck != nil {
			outcome += "；" + map[string]string{"not_reproduced": "复读未再出现慢响应", "slow": "复读仍慢", "read_issue": "复读出现读取异常"}[cell.Recheck.Outcome]
		}
		address := reportAddress(cell.Start, cell.End-cell.Start, job.Disk.LogicalSectorSize)
		value := "未记录"
		if cell.MaxReadLatency != nil {
			address = reportAddress(cell.MaxReadLatency.Offset, int64(cell.MaxReadLatency.Bytes), job.Disk.LogicalSectorSize)
			value = fmt.Sprintf("%.1f ms / %s", cell.MaxReadLatency.Ms, reportBinaryBytes(int64(cell.MaxReadLatency.Bytes)))
		} else if cell.MaxMs != nil {
			value = fmt.Sprintf("%.1f ms（整次读取）", *cell.MaxMs)
		}
		region := cell.Index + 1
		if cell.Index >= 0 && cell.Index < len(r.RepairTargets) {
			region = r.RepairTargets[cell.Index].CellIndex + 1
		}
		v.SlowRows = append(v.SlowRows, reportSlowRow{fmt.Sprintf("%d", region), address, value, outcome})
	}
	if job.Options.ScanRepair != nil && *job.Options.ScanRepair {
		v.Repair, v.RepairThreshold = &s.Repair, 500
		if job.Options.ScanRepairAfterMs != nil {
			v.RepairThreshold = *job.Options.ScanRepairAfterMs
		}
	}
	addReportSmart(&v, r.Diagnostics)
	v.Verdict, v.Explanation, v.Tone = "本次检测未记录读取异常", "请结合检测范围、SMART 信息和实际使用情况评估硬盘状态。", "ok"
	switch {
	case bad.Total > 0:
		v.Verdict, v.Explanation, v.Tone = fmt.Sprintf("发现 %d 个坏块区域", bad.Total), fmt.Sprintf("坏块（慢速）%d 个、坏块（不可读）%d 个。按正常使用要求判断，已不适合继续使用，建议更换硬盘；具体地址见明细。", bad.Slow, bad.Unreadable), "danger"
	case s.SkippedBytes > 0:
		v.Verdict, v.Explanation, v.Tone = fmt.Sprintf("发现 %d 个异常区域", scanAbnormalCellCount(r)), "严重卡读已影响正常使用；已跳过 "+reportBytes(s.SkippedBytes)+" 并继续后续区域。跳过范围未完成读取检测，具体地址见明细。", "danger"
	case s.UnreadableRanges > 0:
		v.Verdict, v.Explanation, v.Tone = "发现持续不可读范围", "部分地址在复读与定位后仍未完整读出。具体范围见明细，不能把整格区域都视为损坏。", "danger"
	case s.ErrorCells > 0 || s.DeviceErrorRanges > 0 || s.UnresolvedRanges > 0 || s.Repair.Failed > 0:
		v.Verdict, v.Explanation, v.Tone = "发现读取、设备或写回异常", "请结合地址明细及错误证据判断原因；连接、控制器异常不等同于介质坏道。", "danger"
	case counts["severe"] > 0:
		v.Verdict, v.Explanation, v.Tone = fmt.Sprintf("发现 %d 个异常区域", scanAbnormalCellCount(r)), "存在严重慢读，已影响正常使用。具体地址和读取耗时见明细。", "danger"
	case job.Status != "completed":
		v.Verdict, v.Explanation, v.Tone = "扫描尚未完成", "本报告记录导出时已完成的检测，未扫描范围没有检测结论。", "warning"
	case v.Legacy:
		v.Verdict, v.Explanation, v.Tone = "历史扫描记录", "保留当时的检测结果；旧记录无法补充后来新增的读取判定和地址证据。", "warning"
	case r.Coverage == "full" && s.CoveredBytes < job.Disk.Size:
		v.Verdict, v.Explanation, v.Tone = "读取判定尚未覆盖整盘", "任务已结束，但保存的读取判定范围未覆盖全部磁盘容量。", "warning"
	case s.RecoveredRanges > 0 || s.SlowCells > 0 || s.Repair.Pending > 0:
		v.Verdict, v.Explanation, v.Tone = "存在慢响应或恢复告警", "请查看响应分布、复读及写回结果；首次出现的慢响应仍保留在统计中。", "warning"
	}
	if r.Diagnostics != nil && (len(r.Diagnostics.Increased) > 0 || (r.Diagnostics.After != nil && (r.Diagnostics.After.Status == "failed" || len(r.Diagnostics.After.Warnings) > 0)) || (r.Diagnostics.Before != nil && (r.Diagnostics.Before.Status == "failed" || len(r.Diagnostics.Before.Warnings) > 0))) && v.Tone == "ok" {
		v.Verdict, v.Explanation, v.Tone = "存在 SMART / 日志告警", "读取扫描未记录异常，但健康计数或设备日志存在告警，请结合下方前后对比评估。", "warning"
	}
	if r.Diagnostics != nil && r.Diagnostics.SelfTest != nil && r.Diagnostics.SelfTest.State == "failed" {
		v.Verdict, v.Explanation, v.Tone = "NVMe 固件自检未通过", "请结合固件自检结果、设备日志及地址明细判断异常原因。", "danger"
	}
	if r.Diagnostics != nil && r.Diagnostics.ReadMethod == "mock" {
		v.Mock = true
		v.Verdict, v.Explanation, v.Tone = "本地演示报告", "本报告使用演示数据，未查询或检测真实硬盘。", "warning"
	}
	if job.Options.ScanRepairFrom != "" && !v.Mock {
		v.Verdict, v.Explanation, v.Tone = "慢区修复处理记录", "仅反映所选历史慢读地址的结果。写回校验通过表示内容一致，慢速改善须经同范围复测确认；原扫描记录保留。", "warning"
		if s.Repair.StillSlow > 0 {
			v.Verdict, v.Explanation, v.Tone = "修复后仍慢", "同范围重复读取仍超出正常使用要求；未反复写入，建议更换硬盘。", "danger"
		}
		if s.Repair.Failed > 0 || s.Repair.Pending > 0 {
			v.Verdict, v.Tone = "存在写回失败或未确定结果", "danger"
		}
	}
	return v
}

func addReportSmart(v *scanHTMLView, d *ScanDiagnostics) {
	if d == nil {
		v.SmartNotes = []string{"本次记录未保存 SMART / 设备日志信息。"}
		return
	}
	stages := []struct {
		name string
		e    *ScanEvidence
	}{{"扫描前", d.Before}, {"扫描后", d.After}}
	keys := map[string]bool{}
	for _, stage := range stages {
		e := stage.e
		if e == nil {
			v.SmartNotes = append(v.SmartNotes, stage.name+"：未保存快照")
			continue
		}
		status := map[string]string{"passed": "健康检查通过", "failed": "健康检查未通过", "unknown": "健康状态未知", "unavailable": "无法读取或不支持"}[e.Status]
		if status == "" || !e.Available {
			status = "健康状态未取得"
		}
		v.SmartNotes = append(v.SmartNotes, stage.name+"："+status)
		if e.Note != "" {
			v.SmartNotes = append(v.SmartNotes, stage.name+"："+e.Note)
		}
		for _, warning := range e.Warnings {
			v.SmartNotes = append(v.SmartNotes, stage.name+"告警："+warning)
		}
		for key := range e.Counters {
			keys[key] = true
		}
	}
	labels := map[string]string{"reallocated": "重映射扇区", "pending": "待处理扇区", "uncorrectable": "离线无法校正扇区", "reported": "已报告不可纠正（厂商原始值）", "nvmeMedia": "NVMe 介质 / 数据完整性错误", "critical": "NVMe 严重警告位", "ataErrors": "ATA 日志错误", "nvmeLog": "NVMe 日志错误", "scsi_read": "SCSI 读取错误"}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		row := reportSmartRow{Label: labels[key], Before: "未取得", After: "未取得", Change: "无法比较"}
		if row.Label == "" {
			row.Label = key
		}
		var before, after int64
		var hasBefore, hasAfter bool
		if d.Before != nil {
			before, hasBefore = d.Before.Counters[key]
		}
		if d.After != nil {
			after, hasAfter = d.After.Counters[key]
		}
		if hasBefore {
			row.Before = fmt.Sprint(before)
		}
		if hasAfter {
			row.After = fmt.Sprint(after)
		}
		if hasBefore && hasAfter {
			row.Change = fmt.Sprintf("%+d", after-before)
			row.Increased = after > before
			if after == before {
				row.Change = "无变化"
			} else if key == "critical" {
				row.Change, row.Increased = "警告位变化", after & ^before != 0
			}
		}
		v.SmartRows = append(v.SmartRows, row)
	}
	for _, increase := range d.Increased {
		v.SmartNotes = append(v.SmartNotes, "扫描期间新增告警："+increase)
	}
	if d.SelfTest != nil {
		state := map[string]string{"passed": "通过", "failed": "未通过", "running": "运行中", "cancelled": "已中止", "unknown": "结果未知"}[d.SelfTest.State]
		v.SmartNotes = append(v.SmartNotes, "NVMe 固件自检："+state+"；"+d.SelfTest.Message)
	}
}

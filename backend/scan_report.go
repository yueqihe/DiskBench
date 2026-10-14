package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// Prepare before HTTP headers; stream the journal without loading it into memory.
func (m *JobManager) prepareScanReport(id string) (func(io.Writer) error, error) {
	job, file, size, err := m.scanIssueSnapshot(id)
	if err != nil {
		return nil, err
	}
	return func(w io.Writer) error {
		if file != nil {
			defer file.Close()
		}
		result := job.Result.(*ScanResult)
		metadata, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "{\"reportVersion\":2,\"legacy\":%t,\"job\":%s,\"issues\":[", result.Version < 2, metadata); err != nil {
			return err
		}
		count := int64(0)
		warning := result.Stats.IssueWarning
		writeRecord := func(encoded []byte) error {
			if count > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if _, err := w.Write(encoded); err != nil {
				return err
			}
			count++
			return nil
		}
		if file == nil {
			if result.Version < 2 {
				warning = "旧报告仅保留原有摘要，不能确认异常地址是否完整"
				for _, r := range result.Stats.BadRanges {
					encoded, err := json.Marshal(r)
					if err != nil {
						return err
					}
					if err := writeRecord(encoded); err != nil {
						return err
					}
				}
			}
		} else {
			scanner := issueScanner(io.LimitReader(file, size))
			for count < result.Stats.SavedIssueCount && scanner.Scan() {
				var record BadRange
				if json.Unmarshal(scanner.Bytes(), &record) != nil || !validScanJournalRecord(record, job.Disk.Size) {
					warning = "异常记录损坏，导出不完整"
					break
				}
				if err := writeRecord(scanner.Bytes()); err != nil {
					return err
				}
			}
			if scanner.Err() != nil || count < result.Stats.SavedIssueCount {
				warning = "异常记录无法完整读取，导出不完整"
			}
		}
		complete := result.Version >= 2 && result.Stats.IssuesComplete && warning == "" && count == result.Stats.IssueCount
		ending, err := json.Marshal(struct {
			Complete bool   `json:"recordsComplete"`
			Count    int64  `json:"exportedIssueCount"`
			Warning  string `json:"exportWarning,omitempty"`
		}{complete, count, warning})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "],%s", ending[1:])
		return err
	}, nil
}

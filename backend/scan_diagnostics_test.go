package main

import "testing"

func TestScanEvidenceSeparatesHealthHistoryAndCurrentRead(t *testing.T) {
	r := parseScanEvidence(`{
	  "smartctl":{"exit_status":0},"smart_status":{"passed":true},
	  "ata_smart_attributes":{"table":[{"id":187,"raw":{"value":25777537024}},{"id":197,"raw":{"value":2}}]},
	  "ata_smart_error_log":{"extended":{"count":3,"table":[
	    {"completion_registers":{"lba":0}}, {"completion_registers":{"lba":100}},
	    {"completion_registers":{"lba":-1}}, {"completion_registers":{"lba":999999999999}}
	  ]}},
	  "ata_smart_self_test_log":{"standard":{"table":[
	    {"status":{"passed":false},"lba":200}, {"status":{"passed":true},"lba":300},
	    {"status":{"passed":false},"lba":"-"}, {"status":{"passed":false},"lba":null}
	  ]}}
	}`, *testDisk())
	if !r.Available || r.Status != "passed" || !r.LogAvailable || len(r.LogHints) != 3 || len(r.Warnings) != 3 {
		t.Fatalf("%+v", r)
	}
	if r.LogHints[0].LBA != 0 || r.LogHints[2].LBA != 200 || r.Counters["reported"] != 25777537024 {
		t.Fatalf("%+v", r)
	}
}

func TestScanEvidenceUnavailableAndProtocolLogs(t *testing.T) {
	for _, output := range []string{"", "not JSON", `{}`} {
		r := parseScanEvidence(output, *testDisk())
		if r.Available || r.LogAvailable || len(r.LogHints) != 0 || r.Note == "" {
			t.Fatalf("%+v", r)
		}
	}
	r := parseScanEvidence(`{"smartctl":{"exit_status":0},
	  "nvme_smart_health_information_log":{"media_errors":4,"critical_warning":0},
	  "nvme_error_information_log":{"logged_count":6,"table":[{"nsid":2,"lba":100}]},
	  "scsi_error_counter_log":{"read":{"total_uncorrected_errors":2}}
	}`, *testDisk())
	if !r.LogAvailable || r.Counters["nvmeMedia"] != 4 || r.Counters["scsi_read"] != 2 || len(r.LogHints) != 0 {
		t.Fatalf("%+v", r)
	}
}

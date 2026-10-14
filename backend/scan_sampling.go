package main

import (
	"crypto/rand"
	"encoding/binary"
)

const stratifiedScanPlan = "stratified-v1"

type ScanSampleRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

func newScanSamplingSeed() uint32 {
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		// Variation is an optimization; repeatability comes from saving the seed.
		return 1
	}
	return binary.LittleEndian.Uint32(seed[:])
}

func sampleRangeBytes(ranges []ScanSampleRange) int64 {
	var total int64
	for _, r := range ranges {
		total += r.End - r.Start
	}
	return total
}

func scanCellTarget(result *ScanResult, cell *ScanCell) int64 {
	if result.ScanMode != "quick" {
		return cell.End - cell.Start
	}
	if len(cell.SampleRanges) > 0 {
		return sampleRangeBytes(cell.SampleRanges)
	}
	if cell.SampleStart == nil || cell.SampleEnd == nil {
		return 0
	}
	return *cell.SampleEnd - *cell.SampleStart
}

// Two non-overlapping strata per cell. Positions change between new scans,
// remain fixed during resume, and include byte zero and the final sector.
func quickSampleRanges(result *ScanResult, cell *ScanCell, diskSize, sector, maxBytes int64) []ScanSampleRange {
	if result.SamplingPlan != stratifiedScanPlan {
		start, length := quickSampleBounds(cell, diskSize, sector, maxBytes)
		return []ScanSampleRange{{Start: start, End: start + length}}
	}
	length := min(cell.End-cell.Start, alignDown(maxBytes, sector))
	if length <= 0 {
		return nil
	}
	if length == cell.End-cell.Start || length < 2*sector {
		return []ScanSampleRange{{Start: cell.Start, End: cell.Start + length}}
	}
	boundary := cell.Start + alignDown((cell.End-cell.Start)/2, sector)
	first := alignDown(length/2, sector)
	sizes := []int64{first, length - first}
	bounds := []int64{cell.Start, boundary, cell.End}
	ranges := make([]ScanSampleRange, 2)
	for i, size := range sizes {
		// Hash the persisted seed and stratum index without platform-sized ints.
		hash := uint64(result.SamplingSeed) + uint64(cell.Index*2+i+1)*0x9e3779b97f4a7c15
		hash = (hash ^ (hash >> 30)) * 0xbf58476d1ce4e5b9
		hash = (hash ^ (hash >> 27)) * 0x94d049bb133111eb
		hash ^= hash >> 31
		slots := (bounds[i+1]-bounds[i]-size)/sector + 1
		start := bounds[i] + int64(hash%uint64(slots))*sector
		if i == 0 && cell.Start == 0 {
			start = 0
		} else if i == 1 && cell.End == diskSize {
			start = diskSize - size
		}
		ranges[i] = ScanSampleRange{Start: start, End: start + size}
	}
	return ranges
}

func setScanSamples(result *ScanResult, cell *ScanCell, diskSize, sector, maxBytes int64) []ScanSampleRange {
	ranges := quickSampleRanges(result, cell, diskSize, sector, maxBytes)
	cell.SampleStart, cell.SampleEnd = intPtr(ranges[0].Start), intPtr(ranges[len(ranges)-1].End)
	if result.SamplingPlan == stratifiedScanPlan {
		// Published slices are immutable: API snapshots may share them.
		cell.SampleRanges = ranges
	}
	return ranges
}

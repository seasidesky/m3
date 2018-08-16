// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package compaction

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/m3db/m3/src/dbnode/storage/index/segments"
)

var (
	errMutableCompactionAgeNegative = errors.New("mutable compaction age must be postive")
	errSizeBucketsUndefined         = errors.New("size buckets are undefined")
	errMaxImmutableCompactionSize   = errors.New("max immutable compaction size must be positive")
)

var (
	DefaultSizeBuckets = []SizeBucket{ // i.e. tiers for compaction [0, 524K), [524K, 2M), [2M, 8M)
		SizeBucket{
			MinSizeInclusive: 0,
			MaxSizeExclusive: 1 << 19,
		},
		SizeBucket{
			MinSizeInclusive: 1 << 19,
			MaxSizeExclusive: 1 << 21,
		},
		SizeBucket{
			MinSizeInclusive: 1 << 21,
			MaxSizeExclusive: 1 << 23,
		},
	}

	DefaultOptions = PlannerOptions{
		MaxImmutableCompactionSize: 1 << 21,                            // ~2M
		MaxMutableSegmentSize:      1 << 16,                            // 64K
		MutableCompactionAge:       15 * time.Second,                   // any mutable segment 15s or older is eligible for compactions
		SizeBuckets:                DefaultSizeBuckets,                 // sizes defined above
		OrderBy:                    TasksOrderedByOldestMutableAndSize, // compact mutable segments first
	}
)

// NewPlan returns a new compaction.Plan per the rules above and the knobs provided.
func NewPlan(candidateSegments []Segment, opts PlannerOptions) (*Plan, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	plan := &Plan{
		OrderBy: opts.OrderBy,
	}
	// Planning is a two-phase process:
	// (1) Identify all "compactable" Segments:
	//    - All mutable segments are compactable (over age Y)
	//    - All immutable segments (below size MaxCompactionSize) are compactable
	//
	// (2) Come up with a logical plan for compactable segments
	//    (a) Group the segments into given buckets (compactions can only be performed for segments within the same bucket)
	//    (b) For each bucket:
	//    (b1) Accumulate segments until cumulative size is over the max of the current bucket.
	//    (b2) Add a Task which comprises segments from (b1) to the Plan.
	//    (b3) Continue (b1) until the bucket is empty.
	//    (c) Priotize Tasks w/ "compactable" Mutable Segments over all others

	// 1st phase - find all compactable segments
	var compactableSegments []Segment
	for _, seg := range candidateSegments {
		compactable := (seg.Type == segments.FSTType && seg.Size < opts.MaxImmutableCompactionSize) ||
			(seg.Type == segments.MutableType &&
				(seg.Age >= opts.MutableCompactionAge || seg.Size >= opts.MaxMutableSegmentSize))
		if compactable {
			compactableSegments = append(compactableSegments, seg)
		} else {
			plan.UnusedSegments = append(plan.UnusedSegments, seg)
		}
	}

	// if we don't have any compactable segments, we can early terminate
	if len(compactableSegments) == 0 {
		return plan, nil
	}

	// now we have segments to compact, so on to phase 2
	buckets := opts.SizeBuckets
	sort.Sort(ByMinSize(buckets))

	// group segments into buckets (2a)
	segmentsByBucket := make(map[SizeBucket][]Segment, len(buckets))
	for _, seg := range compactableSegments {
		var (
			bucket      SizeBucket
			bucketFound bool
		)
		for _, b := range buckets {
			if b.MinSizeInclusive <= seg.Size && seg.Size < b.MaxSizeExclusive {
				bucket = b
				bucketFound = true
				break
			}
		}
		if !bucketFound {
			plan.UnusedSegments = append(plan.UnusedSegments, seg)
		}
		segmentsByBucket[bucket] = append(segmentsByBucket[bucket], seg)
	}

	// for each bucket, sub-group segments into tier'd sizes (2b)
	for bucket, bucketSegments := range segmentsByBucket {
		var (
			task            Task
			accumulatedSize int64
		)
		sort.Slice(bucketSegments, func(i, j int) bool {
			// i.e. order to prefer mutable segments first, and then smaller segments
			iMutable := bucketSegments[i].Type == segments.MutableType
			jMutable := bucketSegments[j].Type == segments.MutableType
			if iMutable != jMutable {
				return iMutable
			}
			return bucketSegments[i].Size < bucketSegments[j].Size
		})
		for _, seg := range bucketSegments {
			accumulatedSize += seg.Size
			task.Segments = append(task.Segments, seg)
			if accumulatedSize >= bucket.MaxSizeExclusive {
				plan.Tasks = append(plan.Tasks, task)
				task = Task{}
				accumulatedSize = 0
			}
		}
		// fall thru cases: no accumulation, so we're good
		if len(task.Segments) == 0 || accumulatedSize == 0 {
			continue
		}

		// in case we never went over accumulated size, but have 2 or more segments, we should still compact them
		if len(task.Segments) > 1 {
			plan.Tasks = append(plan.Tasks, task)
			continue
		}

		// even if we only have a single segment, if its a mutable segment, we should compact it to convert into a FST
		if task.Segments[0].Type == segments.MutableType {
			plan.Tasks = append(plan.Tasks, task)
			continue
		}

		// at this point, we have a single FST segment but don't need to compact it; so mark it as such
		plan.UnusedSegments = append(plan.UnusedSegments, task.Segments[0])
	}

	// now that we have the plan, we priortise the tasks as requested in the opts. (2c)
	sort.Stable(plan)
	return plan, nil
}

func (p *Plan) Len() int      { return len(p.Tasks) }
func (p *Plan) Swap(i, j int) { p.Tasks[i], p.Tasks[j] = p.Tasks[j], p.Tasks[i] }
func (p *Plan) Less(i, j int) bool {
	switch p.OrderBy {
	case TasksOrderedByOldestMutableAndSize:
		fallthrough
	default:
		taskSummaryi, taskSummaryj := p.Tasks[i].Summary(), p.Tasks[j].Summary()
		if taskSummaryi.CumulativeMutableAge != taskSummaryj.CumulativeMutableAge {
			// i.e. put those tasks which have cumulative age greater first
			return taskSummaryi.CumulativeMutableAge > taskSummaryj.CumulativeMutableAge
		}
		if taskSummaryi.NumMutable != taskSummaryj.NumMutable {
			// i.e. put those tasks with more mutable segments first
			return taskSummaryi.NumMutable > taskSummaryj.NumMutable
		}
		// i.e. smaller tasks over bigger ones
		return taskSummaryi.CumulativeSize < taskSummaryj.CumulativeSize
	}
}

func (o PlannerOptions) Validate() error {
	if o.MutableCompactionAge <= 0 {
		return errMutableCompactionAgeNegative
	}
	if o.MaxImmutableCompactionSize <= 0 {
		return errMaxImmutableCompactionSize
	}
	if len(o.SizeBuckets) == 0 {
		return errSizeBucketsUndefined
	}
	sort.Sort(ByMinSize(o.SizeBuckets))
	for i := 0; i < len(o.SizeBuckets); i++ {
		current := o.SizeBuckets[i]
		if current.MaxSizeExclusive <= current.MinSizeInclusive {
			return fmt.Errorf("illegal size buckets definition, MaxSize <= MinSize (%+v)", current)
		}
	}
	return nil
}

// ByMinSizeAsc orders a []SizeBucket by MinSize in ascending order.
type ByMinSize []SizeBucket

func (a ByMinSize) Len() int           { return len(a) }
func (a ByMinSize) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByMinSize) Less(i, j int) bool { return a[i].MinSizeInclusive < a[j].MinSizeInclusive }

// Summary returns the TaskSummary for the given task.
func (t Task) Summary() TaskSummary {
	ts := TaskSummary{}
	for _, s := range t.Segments {
		ts.CumulativeSize += s.Size
		if s.Type == segments.MutableType {
			ts.NumMutable++
			ts.CumulativeMutableAge += s.Age
		} else if s.Type == segments.FSTType {
			ts.NumFST++
		}
	}
	return ts
}

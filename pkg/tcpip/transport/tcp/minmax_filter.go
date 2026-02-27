// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tcp

import "time"

// windowedFilterSample holds a single value with its associated timestamp.
//
// +stateify savable
type windowedFilterSample struct {
	v int64
	t time.Duration
}

// windowedFilter implements the Kathleen Nichols windowed min/max algorithm
// using 3 samples (best, second-best, third-best). The window is divided into
// quarters; samples older than one quarter are promoted based on recency.
//
// +stateify savable
type windowedFilter struct {
	win     time.Duration
	samples [3]windowedFilterSample // 0=best, 1=second, 2=third
}

// reset clears the filter state.
func (f *windowedFilter) reset(val int64, t time.Duration) {
	s := windowedFilterSample{v: val, t: t}
	f.samples = [3]windowedFilterSample{s, s, s}
}

// getBest returns the current best (min or max) value.
func (f *windowedFilter) getBest() int64 {
	return f.samples[0].v
}

// updateMax updates the filter to track a windowed maximum. If val >= current
// best, it replaces all samples. Otherwise it maintains the 3-sample structure
// and expires old samples based on the window.
func (f *windowedFilter) updateMax(t time.Duration, val int64) {
	s := windowedFilterSample{v: val, t: t}

	if val >= f.samples[0].v || t-f.samples[2].t > f.win {
		// New max or everything expired.
		f.samples = [3]windowedFilterSample{s, s, s}
		return
	}

	if val >= f.samples[1].v {
		f.samples[1] = s
		f.samples[2] = s
	} else if val >= f.samples[2].v {
		f.samples[2] = s
	}

	// Expire old subwindow samples.
	quarter := f.win / 4
	if t-f.samples[0].t > f.win {
		// Best is too old; promote second.
		f.samples[0] = f.samples[1]
		f.samples[1] = f.samples[2]
		f.samples[2] = s
		if t-f.samples[0].t > f.win {
			f.samples[0] = f.samples[1]
			f.samples[1] = f.samples[2]
			f.samples[2] = s
		}
	} else if t-f.samples[1].t > f.win-quarter {
		f.samples[1] = f.samples[2]
		f.samples[2] = s
	} else if t-f.samples[2].t > f.win-2*quarter {
		f.samples[2] = s
	}
}

// updateMin updates the filter to track a windowed minimum. If val <= current
// best, it replaces all samples. Otherwise it maintains the 3-sample structure
// and expires old samples based on the window.
func (f *windowedFilter) updateMin(t time.Duration, val int64) {
	s := windowedFilterSample{v: val, t: t}

	if val <= f.samples[0].v || t-f.samples[2].t > f.win {
		// New min or everything expired.
		f.samples = [3]windowedFilterSample{s, s, s}
		return
	}

	if val <= f.samples[1].v {
		f.samples[1] = s
		f.samples[2] = s
	} else if val <= f.samples[2].v {
		f.samples[2] = s
	}

	// Expire old subwindow samples.
	quarter := f.win / 4
	if t-f.samples[0].t > f.win {
		f.samples[0] = f.samples[1]
		f.samples[1] = f.samples[2]
		f.samples[2] = s
		if t-f.samples[0].t > f.win {
			f.samples[0] = f.samples[1]
			f.samples[1] = f.samples[2]
			f.samples[2] = s
		}
	} else if t-f.samples[1].t > f.win-quarter {
		f.samples[1] = f.samples[2]
		f.samples[2] = s
	} else if t-f.samples[2].t > f.win-2*quarter {
		f.samples[2] = s
	}
}

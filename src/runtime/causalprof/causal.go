// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package causalprof implements causal profiles as described by
// https://web.cs.umass.edu/publication/docs/2015/UM-CS-2015-008.pdf
package causalprof

import (
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
)

var cpu struct {
	sync.Mutex
	profiling bool
	done      chan bool
}

// Start enables causal profiling. While running, results of causal profiling experiments will
// be written to w. Start returns an error if causal profiling or CPU profiling is already enabled.
func Start(w io.Writer) error {
	cpu.Lock()
	defer cpu.Unlock()
	if cpu.done == nil {
		cpu.done = make(chan bool)
	}

	if cpu.profiling {
		return fmt.Errorf("causal profiling already in use")
	}

	if pprof.IsCPUProfiling() {
		return fmt.Errorf("cpu profiling already in use")
	}
	cpu.profiling = true
	runtime.SetCPUProfileRate(profilingHz)
	go profileWriter(w)
	return nil
}

const profilingHz = 1000
const delayPerPercent = 1e7 / profilingHz

// Stop stops causal profiling if enabled.
// Stop interrupts any currently running experiment without printing
// its results.
func Stop() {
	cpu.Lock()
	defer cpu.Unlock()

	if !cpu.profiling {
		return
	}
	cpu.profiling = false
	runtime.SetCPUProfileRate(0)
	cpu.done <- true
}

func profileWriter(w io.Writer) {
	experiments := make(map[uintptr][]int)
	hasNullExperiment := false
	for {
		pc := runtime_causalProfileStart()
		if pc == 0 {
			break
		}
		expinfo, ok := experiments[pc]
		if !ok {
			expinfo = rand.Perm(20)
			experiments[pc] = expinfo
		}
		delaypersample := uint64(0)
		if !hasNullExperiment {
			hasNullExperiment = true
		} else {
			exp, expinfo := selectExperiment(expinfo)
			if exp == -1 {
				runtime_causalProfileInstall(0)
				continue
			}
			experiments[pc] = expinfo
			delaypersample = uint64(exp) * (5 * delayPerPercent)
		}
		resetProgress()
		runtime_causalProfileInstall(delaypersample)
		// TODO (dmo): variable sleep
		select {
		case <-time.After(500 * (time.Second / profilingHz)):
		case <-cpu.done:
			runtime_causalProfileInstall(0)
			return
		}
		runtime_causalProfileInstall(0)
		diff := compareprogress()
		_func := runtime.FuncForPC(pc)
		file, line := _func.FileLine(pc)
		fmt.Fprintf(w, "# %s %s:%d\n", _func.Name(), file, line)
		fmt.Fprintf(w, "# speedup %d%%\n", delaypersample/delayPerPercent)
		fmt.Fprintf(w, "# %dns/op\n", diff)
		fmt.Fprintf(w, "%#x %d %d\n", pc, delaypersample/delayPerPercent, diff)
		// allow currently sleeping goroutines to return to normal
		time.Sleep(1000 * (time.Second / profilingHz))
	}
}

func selectExperiment(expinfo []int) (int, []int) {
	if len(expinfo) == 0 {
		return -1, nil
	}
	exp := expinfo[0] + 1
	expinfo = expinfo[1:]
	return exp, expinfo
}

func runtime_causalProfileStart() uintptr
func runtime_causalProfileInstall(delay uint64)
func runtime_causalProfileGetDelay() uint64

var progress int
var progresstime time.Duration
var progressmu sync.Mutex

func resetProgress() {
	progressmu.Lock()
	defer progressmu.Unlock()
	progress = 0
	progresstime = 0
}

type Progress struct {
	startTime  time.Time
	startDelay uint64
}

func StartProgress() Progress {
	return Progress{
		startTime:  time.Now(),
		startDelay: runtime_causalProfileGetDelay(),
	}
}

func (p *Progress) Stop() {
	t := time.Since(p.startTime)
	d := runtime_causalProfileGetDelay() - p.startDelay
	t -= time.Duration(d)
	progressmu.Lock()
	defer progressmu.Unlock()
	progresstime += t
	progress += 1
}

func compareprogress() int {
	progressmu.Lock()
	defer progressmu.Unlock()

	return int(int64(progresstime) / int64(progress))
}

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
	runtime_causalProfileWakeup()
	cpu.done <- true
	// clear out cpu profiling buffers. Profiling will not turn on again until this buffer has been emptied
	for {
		b := runtime.CPUProfile()
		if b == nil {
			break
		}
	}
}

type experiment struct {
	hasNull   bool
	remaining []int
}

func profileWriter(w io.Writer) {
	experiments := make(map[uintptr]*experiment)
	for {
		pc := runtime_causalProfileStart()
		if pc == 0 {
			<-cpu.done
			break
		}
		expinfo, ok := experiments[pc]
		if !ok {
			expinfo = &experiment{
				remaining: rand.Perm(20),
			}
			experiments[pc] = expinfo
		}
		exp := selectExperiment(expinfo)
		if exp == -1 {
			runtime_causalProfileInstall(0)
			continue
		}
		delaypersample := uint64(exp) * (5 * delayPerPercent)

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
		if diff == -1 {
			continue
		}
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

func selectExperiment(expinfo *experiment) int {
	if !expinfo.hasNull && rand.Intn(2) == 1 {
		expinfo.hasNull = true
		return 0
	}
	if len(expinfo.remaining) == 0 {
		if !expinfo.hasNull {
			expinfo.hasNull = true
			return 0
		}
		return -1
	}
	exp := expinfo.remaining[0] + 1
	expinfo.remaining = expinfo.remaining[1:]
	return exp
}

func runtime_causalProfileStart() uintptr
func runtime_causalProfileInstall(delay uint64)
func runtime_causalProfileGetDelay() uint64
func runtime_causalProfileWakeup()

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
	if progress == 0 {
		return -1
	}

	return int(int64(progresstime) / int64(progress))
}

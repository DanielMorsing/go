// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// CPU profiling.
//
// The signal handler for the profiling clock tick adds a new stack trace
// to a log of recent traces. The log is read by a user goroutine that
// turns it into formatted profile data. If the reader does not keep up
// with the log, those writes will be recorded as a count of lost records.
// The actual profile buffer is in profbuf.go.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const maxCPUProfStack = 64

type cpuProfile struct {
	lock mutex
	on   bool     // profiling is on
	log  *profBuf // profile events written here

	// extra holds extra stacks accumulated in addNonGo
	// corresponding to profiling signals arriving on
	// non-Go-created threads. Those stacks are written
	// to log the next time a normal Go thread gets the
	// signal handler.
	// Assuming the stacks are 2 words each (we don't get
	// a full traceback from those threads), plus one word
	// size for framing, 100 Hz profiling would generate
	// 300 words per second.
	// Hopefully a normal Go thread will get the profiling
	// signal at least once every few seconds.
	extra      [1000]uintptr
	numExtra   int
	lostExtra  uint64 // count of frames lost because extra is full
	lostAtomic uint64 // count of frames lost because of being in atomic64 on mips/arm; updated racily
}

var cpuprof cpuProfile

var causalprof struct {
	// variables used to implement causal profiling
	delaypersample uint64  // per sample delay
	curdelay       uint64  // the amount of delays inserted so far
	ignoredelay    uint64  // delays that need to be ignored. Usually from previous experiments
	pc             uintptr // the line that is being experimented on
	once           uint32  // atomic variable to make sure setup is only done once
	wait           note    // init goroutine waits here
}

const (
	noExp uint32 = iota
	setupExp
	hasExp
)

// SetCPUProfileRate sets the CPU profiling rate to hz samples per second.
// If hz <= 0, SetCPUProfileRate turns off profiling.
// If the profiler is on, the rate cannot be changed without first turning it off.
//
// Most clients should use the runtime/pprof package or
// the testing package's -test.cpuprofile flag instead of calling
// SetCPUProfileRate directly.
func SetCPUProfileRate(hz int) {
	// Clamp hz to something reasonable.
	if hz < 0 {
		hz = 0
	}
	if hz > 1000000 {
		hz = 1000000
	}

	lock(&cpuprof.lock)
	if hz > 0 {
		if cpuprof.on || cpuprof.log != nil {
			print("runtime: cannot set cpu profile rate until previous profile has finished.\n")
			unlock(&cpuprof.lock)
			return
		}

		cpuprof.on = true
		cpuprof.log = newProfBuf(1, 1<<17, 1<<14)
		hdr := [1]uint64{uint64(hz)}
		cpuprof.log.write(nil, nanotime(), hdr[:], nil)
		setcpuprofilerate(int32(hz))
	} else if cpuprof.on {
		setcpuprofilerate(0)
		cpuprof.on = false
		cpuprof.addExtra()
		cpuprof.log.close()
	}
	unlock(&cpuprof.lock)
}

// add adds the stack trace to the profile.
// It is called from signal handlers and other limited environments
// and cannot allocate memory or acquire locks that might be
// held at the time of the signal, nor can it use substantial amounts
// of stack.
//go:nowritebarrierrec
func (p *cpuProfile) add(gp *g, stk []uintptr) {
	// Simple cas-lock to coordinate with setcpuprofilerate.
	for !atomic.Cas(&prof.signalLock, 0, 1) {
		osyield()
	}

	if prof.hz != 0 { // implies cpuprof.log != nil
		if p.numExtra > 0 || p.lostExtra > 0 || p.lostAtomic > 0 {
			p.addExtra()
		}
		hdr := [1]uint64{1}
		// Note: write "knows" that the argument is &gp.labels,
		// because otherwise its write barrier behavior may not
		// be correct. See the long comment there before
		// changing the argument here.
		cpuprof.log.write(&gp.labels, nanotime(), hdr[:], stk)
	}

	atomic.Store(&prof.signalLock, 0)
}

// addNonGo adds the non-Go stack trace to the profile.
// It is called from a non-Go thread, so we cannot use much stack at all,
// nor do anything that needs a g or an m.
// In particular, we can't call cpuprof.log.write.
// Instead, we copy the stack into cpuprof.extra,
// which will be drained the next time a Go thread
// gets the signal handling event.
//go:nosplit
//go:nowritebarrierrec
func (p *cpuProfile) addNonGo(stk []uintptr) {
	// Simple cas-lock to coordinate with SetCPUProfileRate.
	// (Other calls to add or addNonGo should be blocked out
	// by the fact that only one SIGPROF can be handled by the
	// process at a time. If not, this lock will serialize those too.)
	for !atomic.Cas(&prof.signalLock, 0, 1) {
		osyield()
	}

	if cpuprof.numExtra+1+len(stk) < len(cpuprof.extra) {
		i := cpuprof.numExtra
		cpuprof.extra[i] = uintptr(1 + len(stk))
		copy(cpuprof.extra[i+1:], stk)
		cpuprof.numExtra += 1 + len(stk)
	} else {
		cpuprof.lostExtra++
	}

	atomic.Store(&prof.signalLock, 0)
}

// addExtra adds the "extra" profiling events,
// queued by addNonGo, to the profile log.
// addExtra is called either from a signal handler on a Go thread
// or from an ordinary goroutine; either way it can use stack
// and has a g. The world may be stopped, though.
func (p *cpuProfile) addExtra() {
	// Copy accumulated non-Go profile events.
	hdr := [1]uint64{1}
	for i := 0; i < p.numExtra; {
		p.log.write(nil, 0, hdr[:], p.extra[i+1:i+int(p.extra[i])])
		i += int(p.extra[i])
	}
	p.numExtra = 0

	// Report any lost events.
	if p.lostExtra > 0 {
		hdr := [1]uint64{p.lostExtra}
		lostStk := [2]uintptr{
			funcPC(_LostExternalCode) + sys.PCQuantum,
			funcPC(_ExternalCode) + sys.PCQuantum,
		}
		p.log.write(nil, 0, hdr[:], lostStk[:])
		p.lostExtra = 0
	}

	if p.lostAtomic > 0 {
		hdr := [1]uint64{p.lostAtomic}
		lostStk := [2]uintptr{
			funcPC(_LostSIGPROFDuringAtomic64) + sys.PCQuantum,
			funcPC(_System) + sys.PCQuantum,
		}
		p.log.write(nil, 0, hdr[:], lostStk[:])
		p.lostAtomic = 0
	}

}

// CPUProfile panics.
// It formerly provided raw access to chunks of
// a pprof-format profile generated by the runtime.
// The details of generating that format have changed,
// so this functionality has been removed.
//
// Deprecated: Use the runtime/pprof package,
// or the handlers in the net/http/pprof package,
// or the testing package's -test.cpuprofile flag instead.
func CPUProfile() []byte {
	panic("CPUProfile no longer available")
}

// Causal profiling helper functions
//
// Causal profiling is initialized through the following steps.
// When we're calling this function, CPU profiling will have been turned on.
// We set the 'once' variable and then go to sleep. Once the profiler gets a signal,
// it will check the variable, set the PC for the callsite we're instrumenting and
// then wake up this goroutine. We use the profiler to select a callsite so that
// we can be certain that the speedup can be realized.
//
// Once we're woken up, we read the PC, figure out which experiment to run
// and then use causalProfileInstall to set the delay per sample.
//
// We end an experiment by setting the delay per sample to 0. At that point,
// everything should function as normal.
//
// All of these variables are checked concurrently by other threads and profiling signals
// so we use atomic variables here.
//go:linkname runtime_causalProfileStart runtime/causalprof.runtime_causalProfileStart
func runtime_causalProfileStart() (pc uintptr) {

	lock(&cpuprof.lock)
	if !cpuprof.on {
		unlock(&cpuprof.lock)
		return 0
	}
	// set up atomic variables so that profiling signals do the slowdown
	atomic.Store64(&causalprof.delaypersample, 0)
	atomic.Storeuintptr(&causalprof.pc, 0)
	atomic.Store(&causalprof.once, 1)

	// ugly: Part of the shutdown process for causal profiling is to send a signal on
	// a note in case we're waiting for a PC. However, if we're not waiting
	// then that note will be flagged and we will fail on double wakeup
	noteclear(&causalprof.wait)

	unlock(&cpuprof.lock)

	// Wait for profiling signal to come and tell us which line to instrument
	notetsleepg(&causalprof.wait, -1)
	noteclear(&causalprof.wait)
	pc = atomic.Loaduintptr(&causalprof.pc)
	return pc
}

//go:linkname runtime_causalProfileInstall runtime/causalprof.runtime_causalProfileInstall
func runtime_causalProfileInstall(delaypersample uint64) {
	atomic.Store64(&causalprof.delaypersample, delaypersample)
	curdelay := atomic.Load64(&causalprof.curdelay)
	if delaypersample == 0 {
		atomic.Store(&causalprof.once, 0)
		atomic.Store64(&causalprof.ignoredelay, curdelay)
	}
}

//go:linkname runtime_causalProfileWakeup runtime/causalprof.runtime_causalProfileWakeup
func runtime_causalProfileWakeup() {
	notewakeup(&causalprof.wait)
}

//go:linkname runtime_causalProfileGetDelay runtime/causalprof.runtime_causalProfileGetDelay
func runtime_causalProfileGetDelay() uint64 {
	return atomic.Load64(&causalprof.curdelay)
}

// stop the profiler and discard the profile buffer atomically.
//
// Causal profiling uses the profiling system, but does not read records
// written by it. After a profile has been stopped, the profiler will let data
// sit in the buffer, waiting for a reader to empty it before it will let profiling
// be turned on again. We have to empty out the buffer and stop the profiler
// atomically, since there's a small window between causal prof stopping and clearing
// the buffer where a profiler might encounter our dirty data.
//
//go:linkname runtime_causalProfileStopProf runtime/causalprof.runtime_causalProfileStopProf
func runtime_causalProfileStopProf() {
	lock(&cpuprof.lock)
	setcpuprofilerate(0)
	cpuprof.on = false
	cpuprof.log = nil
	unlock(&cpuprof.lock)

}

//go:linkname runtime_pprof_runtime_cyclesPerSecond runtime/pprof.runtime_cyclesPerSecond
func runtime_pprof_runtime_cyclesPerSecond() int64 {
	return tickspersecond()
}

// readProfile, provided to runtime/pprof, returns the next chunk of
// binary CPU profiling stack trace data, blocking until data is available.
// If profiling is turned off and all the profile data accumulated while it was
// on has been returned, readProfile returns eof=true.
// The caller must save the returned data and tags before calling readProfile again.
//
//go:linkname runtime_pprof_readProfile runtime/pprof.readProfile
func runtime_pprof_readProfile() ([]uint64, []unsafe.Pointer, bool) {
	lock(&cpuprof.lock)
	log := cpuprof.log
	unlock(&cpuprof.lock)
	data, tags, eof := log.read(profBufBlocking)
	if len(data) == 0 && eof {
		lock(&cpuprof.lock)
		cpuprof.log = nil
		unlock(&cpuprof.lock)
	}
	return data, tags, eof
}

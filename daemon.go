package main

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var logger = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

func logf(format string, args ...any) { logger.Printf(format, args...) }

func cacheDirFile(name string) string { return filepath.Join(cacheDir(), name) }

const (
	defaultLocalMsCxx = 1000.0
	defaultLocalMsC   = 150.0
	defaultObjBytes   = 2 << 20
	scanCost          = 200 * time.Millisecond
	remoteOverhead    = 80 * time.Millisecond
)

type job struct {
	id       uint64
	argv     []string
	cwd      string
	ci       *CompileInfo
	key      string // timing/size history: shared across build dirs
	depsKey  string // input-set history: per build dir (deps often live in the build dir, e.g. FetchContent)
	mode     string
	ident    string
	estLocal time.Duration
	estRaw   int64
	priority bool // fallback from a failed remote attempt; schedule first
	arrived  time.Time
	decided  chan decision
	closed   chan struct{} // launcher went away
}

type decision struct {
	helper *helper // nil = local
}

func (j *job) remoteCandidate() bool {
	return j.ci != nil && j.ci.RemoteOK && !j.ci.IsPCH && j.mode == ModeDistribute && j.ident != "" && !j.priority
}

type sessionStats struct {
	start, last                time.Time
	jobs, local, remote        int
	fallbacks, verifyFails     int
	rxBytes                    int64
	remoteByHelper             map[string]int
}

type daemon struct {
	lock         *os.File // flock'd for the daemon's lifetime; must stay referenced
	traceMu      sync.Mutex
	trace        *os.File
	mu           sync.Mutex
	cfg          *configStore
	files        *fileCache
	hist         *history
	helpers      []*helper
	localSlots   int
	localRunning map[*job]time.Time // expected end
	pending      []*job
	nextID       uint64
	stats        sessionStats
	lastRequest  time.Time
	stopping     bool

	identMu sync.Mutex
	idents  map[string]string

	biMu       sync.Mutex
	buildInfos map[string]*BuildInfo
}

func runDaemon() {
	os.MkdirAll(cacheDir(), 0o755)
	lock, err := os.OpenFile(cacheDirFile("daemon.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		logf("lock: %v", err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(0) // another daemon is running
	}
	os.Remove(socketPath())
	ln, err := net.Listen("unix", socketPath())
	if err != nil {
		logf("listen: %v", err)
		os.Exit(1)
	}
	d := &daemon{
		lock:         lock,
		cfg:          newConfigStore(),
		files:        newFileCache(),
		hist:         loadHistory(cacheDirFile("history.gob")),
		localRunning: map[*job]time.Time{},
		idents:       map[string]string{},
		buildInfos:   map[string]*BuildInfo{},
		lastRequest:  time.Now(),
	}
	g := d.cfg.global()
	d.localSlots = runtime.NumCPU()
	if g.Scheduler.LocalSlots != nil && *g.Scheduler.LocalSlots > 0 {
		d.localSlots = *g.Scheduler.LocalSlots
	}
	for _, hc := range g.Helpers {
		if hc.Enabled != nil && !*hc.Enabled {
			continue
		}
		h := newHelper(d, hc)
		d.helpers = append(d.helpers, h)
		go h.loop()
	}
	d.trace, _ = os.OpenFile(cacheDirFile("trace.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	logf("daemon %s started: %d local slots, %d helpers", version, d.localSlots, len(d.helpers))
	go d.hist.saver()
	go d.ticker()
	for {
		conn, err := ln.Accept()
		if err != nil {
			logf("accept: %v", err)
			continue
		}
		go d.handle(conn)
	}
}

func (d *daemon) shutdown() {
	d.hist.save()
	os.Remove(socketPath())
	os.Exit(0)
}

func (d *daemon) ticker() {
	for range time.Tick(100 * time.Millisecond) {
		d.mu.Lock()
		d.schedule()
		idle := len(d.pending) == 0 && len(d.localRunning) == 0 && d.remoteRunning() == 0
		if idle && d.stats.jobs > 0 && time.Since(d.stats.last) > 3*time.Second {
			d.logSession()
		}
		quit := idle && (d.stopping || time.Since(d.lastRequest) > 2*time.Hour)
		d.mu.Unlock()
		if quit {
			logf("daemon exiting")
			d.shutdown()
		}
	}
}

func (d *daemon) remoteRunning() int {
	n := 0
	for _, h := range d.helpers {
		n += h.running
	}
	return n
}

func (d *daemon) logSession() {
	s := &d.stats
	wall := s.last.Sub(s.start).Round(10 * time.Millisecond)
	var hs []string
	for name, n := range s.remoteByHelper {
		hs = append(hs, fmt.Sprintf("%s=%d", name, n))
	}
	logf("build finished: %d jobs in %v | local %d, remote %d [%s], fallbacks %d, verify-fails %d, received %.1f MB",
		s.jobs, wall, s.local, s.remote, strings.Join(hs, " "), s.fallbacks, s.verifyFails, float64(s.rxBytes)/1e6)
	d.hist.save()
	d.stats = sessionStats{}
}

func (d *daemon) noteJob(f func(s *sessionStats)) {
	d.mu.Lock()
	if d.stats.jobs == 0 && d.stats.start.IsZero() {
		d.stats.start = time.Now()
		d.stats.remoteByHelper = map[string]int{}
	}
	f(&d.stats)
	d.stats.last = time.Now()
	d.mu.Unlock()
}

// traceJob appends one line per finished job to trace.jsonl (for tuning the scheduler).
func (d *daemon) traceJob(j *job, where string, runMs int64, extra map[string]any) {
	if d.trace == nil {
		return
	}
	rec := map[string]any{
		"t": time.Now().UnixMilli(), "arrived": j.arrived.UnixMilli(), "where": where,
		"src": filepath.Base(j.ci.Source), "est_ms": j.estLocal.Milliseconds(), "run_ms": runMs,
	}
	for k, v := range extra {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	d.traceMu.Lock()
	d.trace.Write(append(b, '\n'))
	d.traceMu.Unlock()
}

func (d *daemon) kick() {
	d.mu.Lock()
	d.schedule()
	d.mu.Unlock()
}

func (d *daemon) identFor(compiler string) string {
	d.identMu.Lock()
	defer d.identMu.Unlock()
	st, err := os.Stat(compiler)
	if err != nil {
		return ""
	}
	key := compiler + "|" + st.ModTime().String()
	if id, ok := d.idents[key]; ok {
		return id
	}
	out, err := exec.Command(compiler, "--version").Output()
	id := ""
	if err == nil {
		id = normalizeIdent(string(out))
	}
	d.idents[key] = id
	return id
}

func (d *daemon) buildInfo(cwd string) *BuildInfo {
	d.biMu.Lock()
	defer d.biMu.Unlock()
	if bi, ok := d.buildInfos[cwd]; ok {
		if bi.cacheFile == "" {
			return bi
		}
		if st, err := os.Stat(bi.cacheFile); err == nil && st.ModTime().Equal(bi.mtime) {
			return bi
		}
	}
	bi := findBuildInfo(cwd)
	d.buildInfos[cwd] = bi
	return bi
}

func (d *daemon) prepare(req ipcRequest) *job {
	j := &job{argv: req.Argv, cwd: req.Cwd, arrived: time.Now(), decided: make(chan decision, 1), closed: make(chan struct{})}
	j.ci = parseCompile(req.Argv, req.Cwd)
	bi := d.buildInfo(req.Cwd)
	cfg := d.cfg.forRepo(bi.RepoRoot)
	j.mode = cfg.modeFor(bi.BuildType, j.ci.Debug)
	if j.ci.Source != "" && cfg.forcedLocal(bi.RepoRoot, j.ci.Source) {
		j.mode = ModeLocal
	}
	if j.ci.RemoteOK {
		j.key = j.ci.historyKey()
		j.depsKey = j.cwd + "|" + j.key
		if j.mode == ModeDistribute {
			j.ident = d.identFor(j.ci.Compiler)
		}
	}
	e := histEntry{}
	if j.key != "" {
		e = d.hist.estimate(j.key)
	}
	ms := e.LocalMs
	if ms <= 0 && e.RemoteMs > 0 {
		ms = e.RemoteMs / 1.15
	}
	if ms <= 0 {
		ms = defaultLocalMsCxx
		if j.ci.Lang == "c" {
			ms = defaultLocalMsC
		}
	}
	j.estLocal = time.Duration(ms * float64(time.Millisecond))
	j.estRaw = e.OutBytes
	if j.estRaw <= 0 {
		j.estRaw = defaultObjBytes
	}
	return j
}

func (d *daemon) handle(conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	var req ipcRequest
	if err := dec.Decode(&req); err != nil {
		return
	}
	switch req.Kind {
	case "status":
		enc.Encode(ipcResponse{Kind: "status", Text: d.statusText()})
		return
	case "stop":
		d.mu.Lock()
		d.stopping = true
		d.mu.Unlock()
		enc.Encode(ipcResponse{Kind: "ack"})
		return
	case "compile":
	default:
		return
	}
	d.mu.Lock()
	d.lastRequest = time.Now()
	if req.Version != version && !d.stopping {
		logf("launcher version %s != daemon %s; restarting when idle", req.Version, version)
		d.stopping = true
	}
	stopping := d.stopping
	d.mu.Unlock()
	if stopping {
		enc.Encode(ipcResponse{Kind: "local"})
		dec.Decode(&req)
		return
	}

	j := d.prepare(req)
	// Watch for the launcher's "done" message or disconnect.
	doneCh := make(chan ipcRequest, 1)
	go func() {
		var r ipcRequest
		if err := dec.Decode(&r); err != nil {
			close(j.closed)
			return
		}
		doneCh <- r
	}()

	if j.ci.IsPCH && j.mode == ModeDistribute && j.ci.RemoteOK {
		d.hist.setPCHCmd(j.ci.PCHOut, pchCmd{Argv: j.argv, Cwd: j.cwd})
		d.replicatePCH(j)
	}

	d.noteJob(func(s *sessionStats) { s.jobs++ })
	for {
		d.enqueue(j)
		var dec decision
		select {
		case dec = <-j.decided:
		case <-j.closed:
			d.dequeue(j)
			return
		}
		if dec.helper == nil {
			d.runLocal(j, enc, doneCh)
			return
		}
		dispatched := time.Now()
		res, err := d.runRemote(j, dec.helper)
		if err != nil {
			logf("remote %s on %s failed, falling back to local: %v", filepath.Base(j.ci.Source), dec.helper.cfg.Name, err)
			d.noteJob(func(s *sessionStats) { s.fallbacks++ })
			j.priority = true
			continue
		}
		select {
		case <-j.closed:
			return
		default:
		}
		writeErr := error(nil)
		for path, data := range res.Outputs {
			if writeErr = writeFileAtomic(path, data); writeErr != nil {
				break
			}
		}
		if writeErr != nil {
			logf("writing outputs of %s: %v", filepath.Base(j.ci.Source), writeErr)
			j.priority = true
			continue
		}
		d.noteJob(func(s *sessionStats) { s.remote++; s.remoteByHelper[dec.helper.cfg.Name]++ })
		var outBytes int
		for _, o := range res.Outputs {
			outBytes += len(o)
		}
		d.traceJob(j, dec.helper.cfg.Name, res.DurationMs, map[string]any{"bytes": outBytes, "dispatched": dispatched.UnixMilli()})
		enc.Encode(ipcResponse{Kind: "result", Exit: res.Exit, Stdout: res.Stdout, Stderr: res.Stderr})
		return
	}
}

func (d *daemon) runLocal(j *job, enc *json.Encoder, doneCh chan ipcRequest) {
	release := func() {
		d.mu.Lock()
		delete(d.localRunning, j)
		d.schedule()
		d.mu.Unlock()
	}
	if err := enc.Encode(ipcResponse{Kind: "local"}); err != nil {
		release()
		return
	}
	var done ipcRequest
	select {
	case done = <-doneCh:
	case <-j.closed:
		release()
		return
	}
	release()
	d.noteJob(func(s *sessionStats) { s.local++ })
	d.traceJob(j, "local", done.Ms, map[string]any{"fallback": j.priority})
	if j.key != "" && done.Exit == 0 {
		var outBytes int64
		if st, err := os.Stat(j.ci.ObjAbs); err == nil {
			outBytes = st.Size()
		}
		d.hist.update(j.key, func(e *histEntry) {
			e.LocalMs = ewma(e.LocalMs, float64(done.Ms), 0.5)
			if outBytes > 0 && !j.ci.IsPCH {
				e.OutBytes = outBytes
			}
		})
		if j.ci.DepAbs != "" {
			if data, err := os.ReadFile(j.ci.DepAbs); err == nil {
				d.hist.setDeps(j.depsKey, d.filterDeps(j, parseDepfile(string(data))))
			}
		}
	}
	enc.Encode(ipcResponse{Kind: "ack"})
}

// filterDeps makes depfile entries absolute and drops toolchain files (never synced).
func (d *daemon) filterDeps(j *job, deps []string) []string {
	out := deps[:0]
	for _, p := range deps {
		p = absPath(j.cwd, p)
		if underDir(p, j.ci.ToolRoot) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// depsFor predicts the inputs of a compile: last known depfile, else a local -E -M scan.
func (d *daemon) depsFor(j *job) ([]string, error) {
	if deps := d.hist.deps(j.depsKey); deps != nil {
		return deps, nil
	}
	tmp, err := os.CreateTemp("", "dbuild-scan-*.d")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	cmd := exec.Command(j.ci.Compiler, scanArgs(j.ci, tmp.Name())...)
	cmd.Dir = j.cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("dependency scan: %v: %s", err, firstLine(string(out)))
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	deps := d.filterDeps(j, parseDepfile(string(data)))
	d.hist.setDeps(j.depsKey, deps)
	return deps, nil
}

// compileError picks the first "error:" line out of compiler output.
func compileError(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "error:") {
			return strings.TrimSpace(l)
		}
	}
	return firstLine(s)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (d *daemon) runRemote(j *job, h *helper) (*Result, error) {
	deps, err := d.depsFor(j)
	if err != nil {
		h.release(nil)
		return nil, err
	}
	pchs := []string{}
	pchs = append(pchs, j.ci.PCHUses...)
	res, err := h.call(j, deps, false, pchs)
	if err != nil {
		return nil, err
	}
	if res.Err != "" {
		d.hist.update(j.key, func(e *histEntry) { e.RemoteFails++; e.LastFail = time.Now().Unix() })
		return nil, errors.New(res.Err)
	}
	if res.Exit != 0 {
		if strings.Contains(string(res.Stderr), "file not found") {
			d.hist.setDeps(j.depsKey, nil) // stale input list: rescan next time
		}
		d.hist.update(j.key, func(e *histEntry) { e.RemoteFails++; e.LastFail = time.Now().Unix() })
		return nil, fmt.Errorf("remote exit %d: %s", res.Exit, compileError(string(res.Stderr)))
	}
	if bad := h.verify(res); bad != "" {
		d.noteJob(func(s *sessionStats) { s.verifyFails++ })
		return nil, fmt.Errorf("input mismatch on helper: %s", bad)
	}
	obj := res.Outputs[j.ci.ObjAbs]
	if obj == nil {
		return nil, errors.New("helper returned no object")
	}
	var deps2 []string
	for p := range res.DepHashes {
		deps2 = append(deps2, p)
	}
	sort.Strings(deps2)
	d.hist.setDeps(j.depsKey, deps2)
	d.hist.update(j.key, func(e *histEntry) {
		e.RemoteMs = ewma(e.RemoteMs, float64(res.DurationMs), 0.5)
		e.OutBytes = int64(len(obj))
		e.RemoteFails = 0
	})
	h.learnSpeed(j, res)
	return res, nil
}

// replicatePCH builds the same PCH on every connected helper so remote TUs can use it.
func (d *daemon) replicatePCH(j *job) {
	j.key = j.ci.historyKey()
	j.depsKey = j.cwd + "|" + j.key
	j.ident = d.identFor(j.ci.Compiler)
	if j.ident == "" {
		return
	}
	d.mu.Lock()
	var hs []*helper
	for _, h := range d.helpers {
		if h.connected && !h.identBad[j.ident] && (h.sandbox || !j.ci.CL) {
			h.pch[j.ci.PCHOut] = pchPending
			h.running++
			hs = append(hs, h)
		}
	}
	d.mu.Unlock()
	for _, h := range hs {
		go d.runReplicate(j, h)
	}
}

func (d *daemon) runReplicate(j *job, h *helper) {
	state := pchFailed
	defer func() {
		d.mu.Lock()
		h.pch[j.ci.PCHOut] = state
		d.mu.Unlock()
	}()
	deps, err := d.depsFor(j)
	if err != nil {
		h.release(nil)
		logf("pch replicate %s: %v", filepath.Base(j.ci.PCHOut), err)
		return
	}
	res, err := h.call(j, deps, true, nil)
	switch {
	case err != nil:
		logf("pch replicate %s on %s: %v", filepath.Base(j.ci.PCHOut), h.cfg.Name, err)
	case res.Err != "" || res.Exit != 0:
		logf("pch replicate %s on %s failed: %s %s", filepath.Base(j.ci.PCHOut), h.cfg.Name, res.Err, string(res.Stderr))
	default:
		if bad := h.verify(res); bad != "" {
			logf("pch replicate %s on %s: input mismatch %s", filepath.Base(j.ci.PCHOut), h.cfg.Name, bad)
			return
		}
		state = pchReady
		logf("pch %s ready on %s (%dms)", filepath.Base(j.ci.PCHOut), h.cfg.Name, res.DurationMs)
	}
}

// lazyReplicate re-creates a PCH on a helper that doesn't have it (new connection, daemon
// restart) from the last command that built it. Caller holds d.mu.
func (d *daemon) lazyReplicate(h *helper, pchPath string) bool {
	c, ok := d.hist.pchCmd(pchPath)
	if !ok {
		return false
	}
	if _, err := os.Stat(pchPath); err != nil {
		return false
	}
	pj := &job{argv: c.Argv, cwd: c.Cwd, ci: parseCompile(c.Argv, c.Cwd)}
	if !pj.ci.RemoteOK || !pj.ci.IsPCH || pj.ci.PCHOut != pchPath || pj.ci.CL && !h.sandbox {
		return false
	}
	pj.key = pj.ci.historyKey()
	pj.depsKey = pj.cwd + "|" + pj.key
	h.pch[pchPath] = pchPending
	h.running++
	go func() {
		pj.ident = d.identFor(pj.ci.Compiler)
		d.runReplicate(pj, h)
	}()
	return true
}

func (d *daemon) enqueue(j *job) {
	d.mu.Lock()
	d.pending = append(d.pending, j)
	d.schedule()
	d.mu.Unlock()
}

func (d *daemon) dequeue(j *job) {
	d.mu.Lock()
	for i, p := range d.pending {
		if p == j {
			d.pending = append(d.pending[:i], d.pending[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
}

// timeHeap simulates when local slots become free.
type timeHeap []time.Time

func (h timeHeap) Len() int           { return len(h) }
func (h timeHeap) Less(i, j int) bool { return h[i].Before(h[j]) }
func (h timeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timeHeap) Push(x any)        { *h = append(*h, x.(time.Time)) }
func (h *timeHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// schedule assigns pending jobs, longest first, to whichever of {a local slot, a helper}
// is expected to finish them earliest. Jobs whose best option is "wait for a local slot"
// reserve that slot in the simulation so later jobs see the real queue. Caller holds d.mu.
func (d *daemon) schedule() {
	if len(d.pending) == 0 {
		return
	}
	now := time.Now()
	sort.SliceStable(d.pending, func(a, b int) bool {
		x, y := d.pending[a], d.pending[b]
		if x.priority != y.priority {
			return x.priority
		}
		return x.estLocal > y.estLocal
	})
	slots := timeHeap{}
	for _, end := range d.localRunning {
		if end.Before(now) {
			end = now
		}
		slots = append(slots, end)
	}
	for len(slots) < d.localSlots {
		slots = append(slots, now)
	}
	heap.Init(&slots)
	margin := 150 * time.Millisecond
	if m := d.cfg.global().Scheduler.RemoteMarginMs; m != nil {
		margin = time.Duration(*m) * time.Millisecond
	}

	kept := d.pending[:0]
	for _, j := range d.pending {
		freeAt := slots[0]
		localETA := freeAt.Add(j.estLocal)
		var best *helper
		var bestETA time.Time
		if j.remoteCandidate() {
			for _, h := range d.helpers {
				if !d.eligible(h, j) {
					continue
				}
				if eta := h.eta(j, now); best == nil || eta.Before(bestETA) {
					best, bestETA = h, eta
				}
			}
		}
		switch {
		case best != nil && bestETA.Add(margin).Before(localETA):
			best.dispatch(j, now)
			j.decided <- decision{helper: best}
		case !freeAt.After(now) && len(d.localRunning) < d.localSlots:
			heap.Pop(&slots)
			heap.Push(&slots, now.Add(j.estLocal))
			d.localRunning[j] = now.Add(j.estLocal)
			j.decided <- decision{}
		default:
			heap.Pop(&slots)
			heap.Push(&slots, localETA)
			kept = append(kept, j)
		}
	}
	for i := len(kept); i < len(d.pending); i++ {
		d.pending[i] = nil
	}
	d.pending = kept
}

// eligible reports whether h can take j right now. Caller holds d.mu.
func (d *daemon) eligible(h *helper, j *job) bool {
	if !h.connected || h.running >= h.slots || h.identBad[j.ident] {
		return false
	}
	// clang-cl options look like absolute paths (/O2), so only sandbox helpers can run them.
	if j.ci.CL && !h.sandbox {
		return false
	}
	// Files that keep failing remotely stay local for an hour, then get another try.
	if e := d.hist.get(j.key); e.RemoteFails >= 2 && time.Now().Unix()-e.LastFail < 3600 {
		return false
	}
	for _, p := range j.ci.PCHUses {
		switch h.pch[p] {
		case pchReady, pchPending:
		case pchFailed:
			return false
		default:
			if !d.lazyReplicate(h, p) {
				return false
			}
		}
	}
	return true
}

func (d *daemon) statusText() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "dbuild daemon %s\n", version)
	fmt.Fprintf(&b, "local: %d/%d running, %d pending\n", len(d.localRunning), d.localSlots, len(d.pending))
	for _, h := range d.helpers {
		state := "disconnected"
		if h.connected {
			state = "connected"
		}
		if h.lastErr != "" && !h.connected {
			state += " (" + h.lastErr + ")"
		}
		fmt.Fprintf(&b, "helper %s: %s, %d/%d running, down %.2f MB/s, speed x%.2f, wire ratio %.3f, pch %v\n",
			h.cfg.Name, state, h.running, h.slots, h.bwDown/1e6, h.speed, h.ratio(), h.pchSummary())
	}
	if d.stats.jobs > 0 {
		fmt.Fprintf(&b, "current build: %d jobs, local %d, remote %d, fallbacks %d\n", d.stats.jobs, d.stats.local, d.stats.remote, d.stats.fallbacks)
	}
	return b.String()
}

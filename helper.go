package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type pchState int

const (
	pchUnknown pchState = iota
	pchPending
	pchReady
	pchFailed
)

// helper is the daemon's view of one remote machine. Scheduling fields are guarded by d.mu.
type helper struct {
	d   *daemon
	cfg HelperConfig

	// guarded by d.mu
	connected bool
	sandbox   bool
	slots     int
	running   int
	calls     map[uint64]*remoteCall
	nextID    uint64
	speed     float64 // remote job time / local job time
	bwDown    float64 // wire bytes/s helper -> us
	wireRx    int64
	payloadRx int64
	identBad  map[string]bool
	pch       map[string]pchState
	lastErr   string
	conn      *helperConn
}

type remoteCall struct {
	readyAt  time.Time
	estBytes int64
	done     chan *Result
}

type helperConn struct {
	h      *helper
	ww     *wireWriter
	sendMu sync.Mutex
	index  map[string]string // canonical client path -> hash; guarded by sendMu
	links  map[string]string
	closed chan struct{}
}

var errDisconnected = errors.New("helper disconnected")

func newHelper(d *daemon, cfg HelperConfig) *helper {
	if cfg.Name == "" {
		cfg.Name = cfg.SSH
	}
	if cfg.Speed <= 0 {
		cfg.Speed = 1.15
	}
	if cfg.DownloadMbps <= 0 {
		cfg.DownloadMbps = 18
	}
	return &helper{
		d: d, cfg: cfg,
		calls:    map[uint64]*remoteCall{},
		speed:    cfg.Speed,
		bwDown:   cfg.DownloadMbps * 1e6 / 8,
		identBad: map[string]bool{},
		pch:      map[string]pchState{},
	}
}

// ratio is wire bytes per payload byte for results (learned).
func (h *helper) ratio() float64 {
	if h.payloadRx < 4<<20 {
		return 0.14
	}
	return float64(h.wireRx) / float64(h.payloadRx)
}

func (h *helper) pchSummary() string {
	var parts []string
	for p, s := range h.pch {
		parts = append(parts, fmt.Sprintf("%s=%s", filepath.Base(p), [...]string{"?", "pending", "ready", "failed"}[s]))
	}
	sort.Strings(parts)
	return "[" + strings.Join(parts, " ") + "]"
}

// eta estimates when j's result would be fully received if dispatched now. It simulates
// the download link: results already in flight are sent first in readiness order.
// Caller holds d.mu.
func (h *helper) eta(j *job, now time.Time) time.Time {
	run := time.Duration(float64(j.estLocal)*h.speed) + remoteOverhead
	if !h.d.hist.hasDeps(j.depsKey) {
		run += scanCost
	}
	ready := now.Add(run)
	bytesNew := int64(float64(j.estRaw) * h.ratio())
	type item struct {
		ready time.Time
		bytes int64
		isNew bool
	}
	items := []item{{ready, bytesNew, true}}
	for _, c := range h.calls {
		r := c.readyAt
		if r.Before(now) {
			r = now
		}
		items = append(items, item{r, c.estBytes, false})
	}
	sort.Slice(items, func(a, b int) bool { return items[a].ready.Before(items[b].ready) })
	t := now
	for _, it := range items {
		if it.ready.After(t) {
			t = it.ready
		}
		t = t.Add(time.Duration(float64(it.bytes) / h.bwDown * float64(time.Second)))
		if it.isNew {
			return t
		}
	}
	return t
}

// dispatch reserves a slot for j. Caller holds d.mu.
func (h *helper) dispatch(j *job, now time.Time) {
	h.running++
}

// release frees a slot taken by dispatch/replicate when no call was made (or after it).
func (h *helper) release(id *uint64) {
	h.d.mu.Lock()
	if id != nil {
		delete(h.calls, *id)
	}
	h.running--
	h.d.schedule()
	h.d.mu.Unlock()
}

func (h *helper) call(j *job, deps []string, replicate bool, waitFor []string) (*Result, error) {
	run := time.Duration(float64(j.estLocal)*h.speed) + remoteOverhead
	c := &remoteCall{readyAt: time.Now().Add(run), estBytes: int64(float64(j.estRaw) * h.ratio()), done: make(chan *Result, 1)}
	if replicate {
		c.estBytes = 0
	}
	h.d.mu.Lock()
	conn := h.conn
	if !h.connected || conn == nil {
		h.d.mu.Unlock()
		h.release(nil)
		return nil, errDisconnected
	}
	h.nextID++
	id := h.nextID
	h.calls[id] = c
	h.d.mu.Unlock()
	defer h.release(&id)

	rj := &RemoteJob{
		ID: id, Replicate: replicate,
		Compiler: j.ci.Compiler, ToolRoot: j.ci.ToolRoot, Ident: j.ident,
		Argv: j.ci.Args, Cwd: j.cwd,
		ObjArg: j.ci.ObjArg, ObjAbs: j.ci.ObjAbs,
		DepArg: j.ci.DepArg, DepAbs: j.ci.DepAbs,
		CL: j.ci.CL, PCHOut: j.ci.PCHOut,
		WaitFor: waitFor,
	}
	if err := conn.send(rj, deps); err != nil {
		return nil, err
	}
	timeout := 10 * run
	if timeout < 90*time.Second {
		timeout = 90 * time.Second
	}
	select {
	case r := <-c.done:
		if r.Err != "" && strings.HasPrefix(r.Err, "toolchain:") {
			h.d.mu.Lock()
			h.identBad[j.ident] = true
			h.d.mu.Unlock()
			logf("%s: %s", h.cfg.Name, r.Err)
		}
		return r, nil
	case <-conn.closed:
		return nil, errDisconnected
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out after %v", timeout)
	}
}

// send uploads any inputs the helper doesn't have (by content hash), then the job.
func (c *helperConn) send(rj *RemoteJob, deps []string) error {
	files := c.h.d.files
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	var upBytes int
	var upFiles int
	for _, p := range deps {
		if underDir(p, rj.ToolRoot) || isPCHPath(p) {
			continue
		}
		hash, _, err := files.hash(p)
		if err != nil {
			continue // missing locally: the compile will report it
		}
		cp := canonTop(p, c.links)
		target := cp
		if real := files.diskCase(p); real != p {
			target = canonTop(real, c.links)
		}
		if c.index[target] != hash {
			data, h, err := files.readWithHash(p)
			if err != nil {
				continue
			}
			if err := c.ww.send(&Msg{File: &PutFile{Path: target, Hash: h, Data: data}}); err != nil {
				return err
			}
			c.index[target] = h
			hash = h
			upBytes += len(data)
			upFiles++
		}
		if target != cp {
			rel, err := filepath.Rel(filepath.Dir(cp), target)
			if err != nil {
				continue
			}
			if v := linkIndexValue(hash, rel); c.index[cp] != v {
				if err := c.ww.send(&Msg{File: &PutFile{Path: cp, Hash: v, Link: rel}}); err != nil {
					return err
				}
				c.index[cp] = v
			}
		}
	}
	if upFiles > 0 {
		logf("%s: synced %d files (%.1f MB) for %s", c.h.cfg.Name, upFiles, float64(upBytes)/1e6, filepath.Base(rj.ObjAbs))
	}
	return c.ww.send(&Msg{Job: rj})
}

// verify checks that every mirrored file the helper's compile read matches our current
// copy. Returns "" if fine, else the first offending path.
func (h *helper) verify(res *Result) string {
	if len(res.DepHashes) == 0 {
		return "(no dependency info)"
	}
	h.d.mu.Lock()
	conn := h.conn
	h.d.mu.Unlock()
	for p, rh := range res.DepHashes {
		if strings.HasPrefix(p, "!") {
			return p[1:] + " (outside mirror)"
		}
		if isPCHPath(p) {
			continue
		}
		lh, _, err := h.d.files.hash(p)
		if err != nil || lh != rh {
			if conn != nil {
				conn.sendMu.Lock()
				delete(conn.index, canonTop(p, conn.links))
				conn.sendMu.Unlock()
			}
			return p
		}
	}
	return ""
}

func (h *helper) learnSpeed(j *job, res *Result) {
	e := h.d.hist.get(j.key)
	if e.LocalMs <= 50 || res.DurationMs <= 0 {
		return
	}
	s := float64(res.DurationMs) / e.LocalMs
	if s < 0.2 || s > 6 {
		return
	}
	h.d.mu.Lock()
	h.speed = ewma(h.speed, s, 0.1)
	h.d.mu.Unlock()
}

// countingReader measures download throughput over busy periods of the link.
type countingReader struct {
	r       io.Reader
	h       *helper
	start   time.Time
	last    time.Time
	periodN int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		now := time.Now()
		if now.Sub(cr.last) > 150*time.Millisecond {
			cr.closePeriod()
			cr.start = now
			cr.periodN = 0
		}
		cr.periodN += int64(n)
		cr.last = now
		cr.h.d.mu.Lock()
		cr.h.wireRx += int64(n)
		cr.h.d.mu.Unlock()
	}
	return n, err
}

func (cr *countingReader) closePeriod() {
	dur := cr.last.Sub(cr.start)
	if dur < 400*time.Millisecond || cr.periodN < 256<<10 {
		return
	}
	rate := float64(cr.periodN) / dur.Seconds()
	cr.h.d.mu.Lock()
	cr.h.bwDown = ewma(cr.h.bwDown, rate, 0.3)
	cr.h.d.mu.Unlock()
}

func (h *helper) sshArgs(extra ...string) []string {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4", "-o", "Compression=no"}
	args = append(args, h.cfg.SSHOptions...)
	args = append(args, h.cfg.SSH)
	return append(args, extra...)
}

// ensureBinary makes sure the helper has this exact dbuild build and returns its path
// (relative to the remote home dir).
func (h *helper) ensureBinary() (string, error) {
	out, err := exec.Command("ssh", h.sshArgs("uname -sm")...).Output()
	if err != nil {
		return "", fmt.Errorf("ssh: %v", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return "", fmt.Errorf("unexpected uname output %q", out)
	}
	goos := strings.ToLower(f[0])
	goarch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[f[1]]
	exe, _ := os.Executable()
	local := filepath.Join(filepath.Dir(exe), "dbuild-"+goos+"-"+goarch)
	if _, err := os.Stat(local); err != nil {
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			local = exe
		} else {
			return "", fmt.Errorf("no helper binary %s (run ./build.sh)", local)
		}
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	remote := ".cache/dbuild/bin/dbuild-" + hex.EncodeToString(sum[:6])
	if exec.Command("ssh", h.sshArgs("test -x "+remote)...).Run() == nil {
		return remote, nil
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(data)
	zw.Close()
	cmd := exec.Command("ssh", h.sshArgs(fmt.Sprintf("mkdir -p .cache/dbuild/bin && gzip -dc > %s.tmp && chmod +x %s.tmp && mv %s.tmp %s", remote, remote, remote, remote))...)
	size := gz.Len()
	cmd.Stdin = &gz
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("deploy: %v: %s", err, out)
	}
	logf("%s: deployed %s (%.1f MB)", h.cfg.Name, remote, float64(size)/1e6)
	return remote, nil
}

func clientID() string {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	var b strings.Builder
	for _, r := range host {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "client"
	}
	return b.String()
}

func (h *helper) loop() {
	backoff := 2 * time.Second
	for {
		started := time.Now()
		err := h.session()
		h.d.mu.Lock()
		h.connected = false
		h.conn = nil
		h.lastErr = err.Error()
		h.pch = map[string]pchState{}
		h.d.mu.Unlock()
		logf("%s: disconnected: %v", h.cfg.Name, err)
		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second
		}
		time.Sleep(backoff)
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (h *helper) session() error {
	bin, err := h.ensureBinary()
	if err != nil {
		return err
	}
	remoteCmd := bin + " serve --client " + clientID()
	for _, t := range h.cfg.Toolchains {
		remoteCmd += " --toolchain '" + t + "'"
	}
	cmd := exec.Command("ssh", h.sshArgs(remoteCmd)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = &prefixWriter{prefix: h.cfg.Name + ": "}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	conn := &helperConn{h: h, ww: newWireWriter(stdin), links: rootLinks(), closed: make(chan struct{})}
	defer close(conn.closed)
	cr := &countingReader{r: stdout, h: h}
	wr, err := newWireReader(bufio.NewReaderSize(cr, 256<<10))
	if err != nil {
		return err
	}
	defer wr.close()
	if err := conn.ww.send(&Msg{Hello: &Hello{Version: version, ClientID: clientID(), TopLinks: conn.links}}); err != nil {
		return err
	}
	m, err := wr.recv()
	if err != nil {
		return fmt.Errorf("handshake: %v", err)
	}
	if m.HelloAck == nil {
		return errors.New("handshake: unexpected message")
	}
	conn.index = m.HelloAck.Index
	if conn.index == nil {
		conn.index = map[string]string{}
	}
	h.d.mu.Lock()
	h.slots = m.HelloAck.Slots
	if h.cfg.Slots > 0 {
		h.slots = h.cfg.Slots
	}
	h.conn = conn
	h.sandbox = m.HelloAck.Sandbox
	h.connected = true
	h.lastErr = ""
	h.d.mu.Unlock()
	mode := "path-rewrite mode (absolute #includes will fall back to local)"
	if m.HelloAck.Sandbox {
		mode = "sandbox mode"
	}
	logf("%s: connected, %s, %d slots, %d mirrored files", h.cfg.Name, mode, h.slots, len(conn.index))

	for {
		m, err := wr.recv()
		if err != nil {
			return err
		}
		if r := m.Result; r != nil {
			n := int64(len(r.Stdout) + len(r.Stderr))
			for _, o := range r.Outputs {
				n += int64(len(o))
			}
			h.d.mu.Lock()
			h.payloadRx += n
			c := h.calls[r.ID]
			h.d.stats.rxBytes += n
			h.d.mu.Unlock()
			if c != nil {
				c.done <- r
			}
		}
	}
}

type prefixWriter struct {
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		logf("%s%s", w.prefix, w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

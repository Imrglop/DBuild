package main

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// server is the helper side: it mirrors client files by content hash and runs compiles.
type server struct {
	mirror    string
	indexPath string
	ww        *wireWriter
	sem       chan struct{}

	sandbox  bool
	links    map[string]string // client top-level symlinks
	rootEnts []os.DirEntry

	mu       sync.Mutex
	index    map[string]string // canonical client path -> hash
	dirty    bool
	inflight map[string]chan struct{} // mirrored PCH path -> closed when its replicate finishes

	tcMu       sync.Mutex
	extraRoots []string
	identCache map[string]string // root -> ident
	tcByIdent  map[string]string
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	client := fs.String("client", "client", "client id")
	slots := fs.Int("slots", runtime.NumCPU(), "parallel compiles")
	var toolchains stringList
	fs.Var(&toolchains, "toolchain", "extra toolchain root glob")
	fs.Parse(args)

	logger.SetPrefix("[serve] ")
	base := filepath.Join(cacheDir(), "mirror")
	s := &server{
		mirror:     filepath.Join(base, *client),
		indexPath:  filepath.Join(base, *client+".index"),
		sem:        make(chan struct{}, *slots),
		index:      map[string]string{},
		inflight:   map[string]chan struct{}{},
		extraRoots: toolchains,
		identCache: map[string]string{},
		tcByIdent:  map[string]string{},
	}
	os.MkdirAll(s.mirror, 0o755)
	s.loadIndex()
	s.sandbox = sandboxAvailable()
	s.rootEnts, _ = os.ReadDir("/")
	s.ww = newWireWriter(os.Stdout)
	wr, err := newWireReader(bufio.NewReaderSize(os.Stdin, 256<<10))
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}
	go func() {
		for range time.Tick(5 * time.Second) {
			s.saveIndex()
		}
	}()
	err = s.serve(wr, *slots)
	s.saveIndex()
	if err != nil && !errors.Is(err, os.ErrClosed) && err.Error() != "EOF" {
		logf("exit: %v", err)
	}
}

func (s *server) loadIndex() {
	f, err := os.Open(s.indexPath)
	if err != nil {
		return
	}
	defer f.Close()
	var idx map[string]string
	if gob.NewDecoder(f).Decode(&idx) == nil {
		// Drop entries whose mirrored file is gone.
		for p, h := range idx {
			if _, err := os.Stat(s.mirror + p); err == nil {
				s.index[p] = h
			}
		}
	}
}

func (s *server) saveIndex() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	var buf bytes.Buffer
	gob.NewEncoder(&buf).Encode(s.index)
	s.mu.Unlock()
	tmp := s.indexPath + ".tmp"
	if os.WriteFile(tmp, buf.Bytes(), 0o644) == nil {
		os.Rename(tmp, s.indexPath)
	}
}

func (s *server) serve(wr *wireReader, slots int) error {
	m, err := wr.recv()
	if err != nil {
		return err
	}
	if m.Hello == nil {
		return errors.New("expected hello")
	}
	s.links = m.Hello.TopLinks
	s.mu.Lock()
	idx := make(map[string]string, len(s.index))
	for k, v := range s.index {
		idx[k] = v
	}
	s.mu.Unlock()
	if err := s.ww.send(&Msg{HelloAck: &HelloAck{Version: version, Sandbox: s.sandbox, Slots: slots, Index: idx}}); err != nil {
		return err
	}
	for {
		m, err := wr.recv()
		if err != nil {
			return err
		}
		switch {
		case m.File != nil:
			s.putFile(m.File)
		case m.Job != nil:
			j := m.Job
			if j.Replicate {
				s.mu.Lock()
				s.inflight[s.real(j.PCHOut)] = make(chan struct{})
				s.mu.Unlock()
			}
			go s.runJob(j)
		}
	}
}

func (s *server) putFile(f *PutFile) {
	dst := s.mirror + f.Path
	if f.Link != "" {
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.Remove(dst)
		if err := os.Symlink(f.Link, dst); err != nil {
			logf("symlink %s: %v", dst, err)
			return
		}
	} else if err := writeFileAtomic(dst, f.Data); err != nil {
		logf("write %s: %v", dst, err)
		return
	}
	s.mu.Lock()
	s.index[f.Path] = f.Hash
	s.dirty = true
	s.mu.Unlock()
}

func (s *server) candidateRoots() []string {
	home := homeDir()
	globs := []string{
		filepath.Join(home, "Android/Sdk/ndk/*/toolchains/llvm/prebuilt/*"),
		filepath.Join(home, "Library/Android/sdk/ndk/*/toolchains/llvm/prebuilt/*"),
	}
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			globs = append(globs, filepath.Join(v, "ndk/*/toolchains/llvm/prebuilt/*"))
		}
	}
	if v := os.Getenv("ANDROID_NDK_HOME"); v != "" {
		globs = append(globs, filepath.Join(v, "toolchains/llvm/prebuilt/*"))
	}
	globs = append(globs, s.extraRoots...)
	var roots []string
	for _, g := range globs {
		m, _ := filepath.Glob(g)
		roots = append(roots, m...)
	}
	return roots
}

func (s *server) toolchain(ident string) (string, error) {
	s.tcMu.Lock()
	defer s.tcMu.Unlock()
	if r, ok := s.tcByIdent[ident]; ok {
		return r, nil
	}
	for _, root := range s.candidateRoots() {
		id, ok := s.identCache[root]
		if !ok {
			out, err := exec.Command(filepath.Join(root, "bin", "clang"), "--version").Output()
			if err == nil {
				id = normalizeIdent(string(out))
			}
			s.identCache[root] = id
		}
		if id == ident {
			s.tcByIdent[ident] = root
			return root, nil
		}
	}
	return "", fmt.Errorf("toolchain: no compiler matching %q", ident)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

// real maps a client path to where the mirror stores it on this machine.
func (s *server) real(clientPath string) string {
	return s.mirror + canonTop(clientPath, s.links)
}

func sandboxAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false
	}
	return exec.Command("bwrap", "--ro-bind", "/", "/", "--", "/bin/true").Run() == nil
}

// Top-level dirs of the helper that client paths may never shadow.
var essentialTops = map[string]bool{"usr": true, "bin": true, "sbin": true, "lib": true, "lib64": true, "etc": true, "proc": true, "dev": true, "sys": true, "run": true}

// sandboxArgs builds a bwrap command line giving the compile a filesystem where client
// paths exist verbatim: the mirror's top-level dirs are mounted at /, the helper's
// toolchain is mounted at the client's toolchain path, and the rest of the helper's root
// is visible read-only.
func (s *server) sandboxArgs(j *RemoteJob, helperRoot string) ([]string, error) {
	clientTops := map[string]bool{}
	ents, _ := os.ReadDir(s.mirror)
	for _, e := range ents {
		if _, isLink := s.links[e.Name()]; e.IsDir() && !isLink {
			clientTops[e.Name()] = true
		}
	}
	for _, p := range []string{j.Cwd, j.ToolRoot} {
		top, _, _ := strings.Cut(strings.TrimPrefix(canonTop(p, s.links), "/"), "/")
		if essentialTops[top] {
			return nil, fmt.Errorf("client path %s shadows helper /%s", p, top)
		}
		clientTops[top] = true
		os.MkdirAll(filepath.Join(s.mirror, top), 0o755)
	}
	args := []string{"--die-with-parent", "--tmpfs", "/"}
	for _, e := range s.rootEnts {
		n := e.Name()
		if clientTops[n] || n == "proc" || n == "dev" {
			continue
		}
		if _, isLink := s.links[n]; isLink && !essentialTops[n] {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			t, err := os.Readlink("/" + n)
			if err == nil {
				args = append(args, "--symlink", t, "/"+n)
			}
			continue
		}
		args = append(args, "--ro-bind", "/"+n, "/"+n)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	for t := range clientTops {
		if essentialTops[t] {
			continue
		}
		args = append(args, "--bind", filepath.Join(s.mirror, t), "/"+t)
	}
	for n, t := range s.links {
		if !essentialTops[n] && !clientTops[n] {
			args = append(args, "--symlink", t, "/"+n)
		}
	}
	if helperRoot != j.ToolRoot {
		args = append(args, "--ro-bind", helperRoot, canonTop(j.ToolRoot, s.links))
	}
	return append(args, "--chdir", j.Cwd, "--"), nil
}

// Longest first so "-isystem-after" isn't read as "-isystem" + "-after".
var includeDirFlags = []string{"-isystem-after", "-cxx-isystem", "-idirafter", "-isystem", "-iquote", "-I", "-F"}
var includeDirFlagsCL = []string{"/external:I", "-external:I", "/imsvc", "-imsvc", "-isystem", "-iquote", "/I", "-I"}

// includeDirs returns include directories exactly as spelled on the command line.
func includeDirs(args []string, cl bool) []string {
	flags := includeDirFlags
	if cl {
		flags = includeDirFlagsCL
	}
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if cl && a == "--" {
			break
		}
		for _, f := range flags {
			if a == f {
				if i+1 < len(args) {
					i++
					out = append(out, args[i])
				}
				break
			}
			if strings.HasPrefix(a, f) && !strings.HasPrefix(a[len(f):], "-") {
				out = append(out, a[len(f):])
				break
			}
		}
	}
	return out
}

// ensureLiteralDirs creates every directory a path names before resolving it, so spellings
// like <dep>/build/cmake/../../lib work even though only files actually read are mirrored
// (Linux resolves ".." physically and needs build/cmake to exist).
func (s *server) ensureLiteralDirs(cwd, p string) {
	if !filepath.IsAbs(p) {
		p = cwd + "/" + p
	}
	if !strings.Contains(p, "/..") {
		return
	}
	cur := ""
	for _, part := range strings.Split(p, "/") {
		if part == "" {
			continue
		}
		cur += "/" + part
		if part == "." || part == ".." {
			continue
		}
		os.Mkdir(s.real(cur), 0o755)
	}
}

func (s *server) runJob(j *RemoteJob) {
	start := time.Now()
	res := &Result{ID: j.ID, Outputs: map[string][]byte{}, DepHashes: map[string]string{}}
	defer func() {
		res.DurationMs = time.Since(start).Milliseconds()
		if j.Replicate {
			s.mu.Lock()
			if ch := s.inflight[s.real(j.PCHOut)]; ch != nil {
				close(ch)
				delete(s.inflight, s.real(j.PCHOut))
			}
			s.mu.Unlock()
		}
		if err := s.ww.send(&Msg{Result: res}); err != nil {
			logf("send result: %v", err)
			os.Exit(1)
		}
	}()
	root, err := s.toolchain(j.Ident)
	if err != nil {
		res.Err = err.Error()
		return
	}
	for _, p := range j.WaitFor {
		s.mu.Lock()
		ch := s.inflight[s.real(p)]
		s.mu.Unlock()
		if ch != nil {
			<-ch
		}
	}
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	start = time.Now()
	if j.CL && !s.sandbox {
		res.Err = "clang-cl compiles need a sandbox helper"
		return
	}
	for _, d := range includeDirs(j.Argv, j.CL) {
		if !underDir(d, j.ToolRoot) {
			s.ensureLiteralDirs(j.Cwd, d)
		}
	}

	// Paths as the compiler sees them (nsX) and where they really are on this machine (realX).
	var cmd *exec.Cmd
	var m pathMapper
	nsCwd, nsObj, nsDep := j.Cwd, j.ObjAbs, j.DepAbs
	argv := j.Argv
	if j.DepArg == "" {
		nsDep = j.ObjAbs + ".dbuild.d"
		argv = withDepfileFlags(argv, j.CL, nsDep)
	}
	realObj, realDep := s.real(nsObj), s.real(nsDep)
	if s.sandbox {
		sb, err := s.sandboxArgs(j, root)
		if err != nil {
			res.Err = err.Error()
			return
		}
		sb = append(sb, j.Compiler)
		cmd = exec.Command("bwrap", append(sb, argv...)...)
		os.MkdirAll(s.real(j.Cwd), 0o755)
	} else {
		m = pathMapper{mirror: s.mirror, clientRoot: j.ToolRoot, helperRoot: root, links: s.links}
		nsCwd = m.path(j.Cwd)
		os.MkdirAll(nsCwd, 0o755)
		// Prefix maps first so any maps from the project itself (later on the command line) win.
		args := []string{
			"-ffile-prefix-map=" + s.mirror + "/=/",
			"-ffile-prefix-map=" + root + "=" + j.ToolRoot,
		}
		for _, a := range argv {
			args = append(args, m.arg(a))
		}
		cmd = exec.Command(m.path(j.Compiler), args...)
		cmd.Dir = nsCwd
	}
	os.MkdirAll(filepath.Dir(realObj), 0o755)
	os.MkdirAll(filepath.Dir(realDep), 0o755)
	unmap := func(b []byte) []byte {
		if s.sandbox {
			return b
		}
		return []byte(m.unmap(string(b)))
	}

	var so, se bytes.Buffer
	aliased := map[string]bool{}
	for attempt := 0; ; attempt++ {
		so.Reset()
		se.Reset()
		c := exec.Command(cmd.Path, cmd.Args[1:]...)
		c.Dir = cmd.Dir
		c.Stdout, c.Stderr = &so, &se
		err = c.Run()
		res.Exit = exitCode(err)
		if res.Exit == 0 || attempt >= 64 || !s.addCaseAlias(j, string(unmap(se.Bytes())), aliased) {
			break
		}
	}
	if res.Exit == -1 {
		res.Err = err.Error()
	}
	res.Stdout = unmap(so.Bytes())
	res.Stderr = unmap(se.Bytes())
	if res.Exit != 0 {
		return
	}
	depData, err := os.ReadFile(realDep)
	if err != nil {
		res.Err = "depfile: " + err.Error()
		return
	}
	depData = unmap(depData)
	s.mu.Lock()
	for _, p := range parseDepfile(string(depData)) {
		p = absPath(j.Cwd, p)
		if underDir(p, j.ToolRoot) {
			continue
		}
		c := canonTop(p, s.links)
		if h, ok := s.aliasedHash(c); ok {
			res.DepHashes[p] = h
		} else if _, err := os.Stat(s.mirror + c); err == nil {
			res.DepHashes[p] = "" // produced on the helper (e.g. a replicated PCH)
		} else {
			res.DepHashes["!"+p] = ""
		}
	}
	s.mu.Unlock()
	if j.DepArg != "" {
		res.Outputs[j.DepAbs] = depData
	}
	os.Remove(realDep)
	if !j.Replicate {
		obj, err := os.ReadFile(realObj)
		if err != nil {
			res.Err = "object: " + err.Error()
			return
		}
		res.Outputs[j.ObjAbs] = obj
		os.Remove(realObj)
	}
}

// "<includer>(L,C): fatal error: 'x.h' file not found" (cl) or "<includer>:L:C: ..." (gcc).
var notFoundRe = regexp.MustCompile(`(?m)^(.+?)(?:\(\d+,\d+\)|:\d+:\d+): fatal error: '([^']+)' file not found`)

// addCaseAlias handles a compile that failed on an #include the client's case-insensitive
// filesystem resolves by case-folding (e.g. <libhat/Access.hpp> for access.hpp). Depfiles
// name only one spelling of each file, so the client can't sync the others up front. If the
// mirror has exactly that file under other case, link the spelling to it and report true so
// the compile is retried. Verification still checks the content the compile read. tried
// holds this job's earlier misses, so a spelling that still fails after its fix isn't retried.
func (s *server) addCaseAlias(j *RemoteJob, stderr string, tried map[string]bool) bool {
	m := notFoundRe.FindStringSubmatch(stderr)
	if m == nil {
		return false
	}
	includer, spelled := absPath(j.Cwd, m[1]), m[2]
	if tried[m[1]+"|"+spelled] {
		return false
	}
	tried[m[1]+"|"+spelled] = true
	dirs := []string{filepath.Dir(includer)}
	for _, d := range includeDirs(j.Argv, j.CL) {
		dirs = append(dirs, absPath(j.Cwd, d))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range dirs {
		alias := canonTop(filepath.Join(d, spelled), s.links)
		if underDir(alias, j.ToolRoot) {
			continue
		}
		target := s.foldLookup(alias)
		if target == "" {
			continue
		}
		if target == alias {
			return true // another compile linked it meanwhile
		}
		h, ok := s.index[target]
		if !ok {
			continue
		}
		rel, err := filepath.Rel(filepath.Dir(alias), target)
		if err != nil {
			continue
		}
		dst := s.mirror + alias
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.Remove(dst)
		if err := os.Symlink(rel, dst); err != nil {
			logf("case alias %s: %v", alias, err)
			return false
		}
		s.index[alias] = linkIndexValue(indexHash(h), rel)
		s.dirty = true
		return true
	}
	return false
}

// aliasedHash returns the content hash mirrored at client path c, following a case alias to
// its target so the result tracks later re-syncs of the target. Caller holds s.mu.
func (s *server) aliasedHash(c string) (string, bool) {
	v, ok := s.index[c]
	if !ok {
		return "", false
	}
	if l, isLink := strings.CutPrefix(v, "L"); isLink {
		if _, rel, ok := strings.Cut(l, "|"); ok {
			if t, ok := s.index[filepath.Join(filepath.Dir(c), rel)]; ok {
				return indexHash(t), true
			}
		}
	}
	return indexHash(v), true
}

// foldLookup finds the mirrored file matching client path p case-insensitively, preferring
// exact matches per component. Returns its client path, or "".
func (s *server) foldLookup(p string) string {
	cur := ""
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		next := cur + "/" + part
		if _, err := os.Lstat(s.mirror + next); err != nil {
			ents, err := os.ReadDir(s.mirror + cur)
			if err != nil {
				return ""
			}
			found := ""
			for _, e := range ents {
				if strings.EqualFold(e.Name(), part) {
					if found != "" {
						return "" // ambiguous
					}
					found = e.Name()
				}
			}
			if found == "" {
				return ""
			}
			next = cur + "/" + found
		}
		cur = next
	}
	if st, err := os.Stat(s.mirror + cur); err != nil || st.IsDir() {
		return ""
	}
	return cur
}

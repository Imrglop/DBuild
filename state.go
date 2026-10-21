package main

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}

// fileCache hashes files, reusing the hash while size+mtime are unchanged.
type fileCache struct {
	mu      sync.Mutex
	entries map[string]fileEntry
	cased   map[string]string // path as spelled -> as stored on disk, when they differ only in case
}

type fileEntry struct {
	size  int64
	mtime int64
	hash  string
}

func newFileCache() *fileCache {
	return &fileCache{entries: map[string]fileEntry{}, cased: map[string]string{}}
}

// diskCase returns p with each component spelled as stored on disk, or p itself when the
// filesystem is case-sensitive, the spelling already matches, or the lookup fails. On a
// case-insensitive client, headers included as both DirectXMath.h and directxmath.h are one
// file; the helper must see one file too, or #pragma once breaks.
func (c *fileCache) diskCase(p string) string {
	c.mu.Lock()
	r, ok := c.cased[p]
	c.mu.Unlock()
	if ok {
		return r
	}
	r = p
	if d, err := diskPath(p); err == nil && d != p {
		r = fixCase(p, d)
	}
	c.mu.Lock()
	c.cased[p] = r
	c.mu.Unlock()
	return r
}

// fixCase respells p's trailing components as in d (p's on-disk path) for as long as they
// match case-insensitively; d may differ further up where p goes through symlinks.
func fixCase(p, d string) string {
	ps, ds := strings.Split(p, "/"), strings.Split(d, "/")
	for i, j := len(ps)-1, len(ds)-1; i >= 0 && j >= 0 && strings.EqualFold(ps[i], ds[j]); i, j = i-1, j-1 {
		ps[i] = ds[j]
	}
	return strings.Join(ps, "/")
}

// Helper index values for case aliases: "L" + content hash + "|" + relative symlink target.
func linkIndexValue(hash, target string) string { return "L" + hash + "|" + target }

// indexHash returns the content hash an index value stands for.
func indexHash(v string) string {
	if h, ok := strings.CutPrefix(v, "L"); ok {
		h, _, _ = strings.Cut(h, "|")
		return h
	}
	return v
}

func (c *fileCache) hash(path string) (string, int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	mt := st.ModTime().UnixNano()
	c.mu.Lock()
	e, ok := c.entries[path]
	c.mu.Unlock()
	if ok && e.size == st.Size() && e.mtime == mt {
		return e.hash, e.size, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	h := hashBytes(data)
	c.mu.Lock()
	c.entries[path] = fileEntry{size: st.Size(), mtime: mt, hash: h}
	c.mu.Unlock()
	return h, st.Size(), nil
}

// readWithHash reads a file and returns its content and hash, refreshing the cache.
func (c *fileCache) readWithHash(path string) ([]byte, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	h := hashBytes(data)
	c.mu.Lock()
	c.entries[path] = fileEntry{size: st.Size(), mtime: st.ModTime().UnixNano(), hash: h}
	c.mu.Unlock()
	return data, h, nil
}

// history remembers per-compile timings, output sizes and input sets across builds.
type history struct {
	mu       sync.Mutex
	path     string
	dirty    bool
	Paths    []string
	pathIdx  map[string]uint32
	Entries  map[string]*histEntry
	BySource map[string]*histEntry // latest timing/size per source file, any flags or target
	PCHCmds  map[string]pchCmd     // PCH output path -> last command that built it
}

type histEntry struct {
	LocalMs     float64
	RemoteMs    float64
	OutBytes    int64
	Deps        []uint32
	RemoteFails int
	LastFail    int64 // unix seconds of the last remote failure
	Updated     int64
}

type pchCmd struct {
	Argv []string
	Cwd  string
}

func loadHistory(path string) *history {
	h := &history{path: path, pathIdx: map[string]uint32{}, Entries: map[string]*histEntry{}, BySource: map[string]*histEntry{}, PCHCmds: map[string]pchCmd{}}
	f, err := os.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()
	var disk history
	if err := gob.NewDecoder(f).Decode(&disk); err != nil {
		logf("history: %v (starting fresh)", err)
		return h
	}
	h.Paths = disk.Paths
	if disk.Entries != nil {
		h.Entries = disk.Entries
	}
	if disk.PCHCmds != nil {
		h.PCHCmds = disk.PCHCmds
	}
	if disk.BySource != nil {
		h.BySource = disk.BySource
	}
	for i, p := range h.Paths {
		h.pathIdx[p] = uint32(i)
	}
	return h
}

func (h *history) save() {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return
	}
	h.dirty = false
	tmp := h.path + ".tmp"
	f, err := os.Create(tmp)
	if err == nil {
		err = gob.NewEncoder(f).Encode(h)
		f.Close()
	}
	h.mu.Unlock()
	if err == nil {
		os.Rename(tmp, h.path)
	} else {
		logf("history save: %v", err)
	}
}

func (h *history) saver() {
	for range time.Tick(15 * time.Second) {
		h.save()
	}
}

func (h *history) get(key string) histEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.Entries[key]; e != nil {
		return *e
	}
	return histEntry{}
}

// estimate returns timing/size for key, falling back to the same source file built with
// other flags or for another target (e.g. a new ABI or optimization level), so a first
// build of a new configuration still schedules big files first and remotely.
func (h *history) estimate(key string) histEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.Entries[key]; e != nil && (e.LocalMs > 0 || e.RemoteMs > 0) {
		return *e
	}
	src, _, _ := strings.Cut(key, "|")
	if e := h.BySource[src]; e != nil {
		return histEntry{LocalMs: e.LocalMs, RemoteMs: e.RemoteMs, OutBytes: e.OutBytes}
	}
	return histEntry{}
}

func (h *history) deps(key string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.Entries[key]
	if e == nil || len(e.Deps) == 0 {
		return nil
	}
	out := make([]string, len(e.Deps))
	for i, d := range e.Deps {
		out[i] = h.Paths[d]
	}
	return out
}

func (h *history) hasDeps(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.Entries[key]
	return e != nil && len(e.Deps) > 0
}

func (h *history) update(key string, fn func(e *histEntry)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.Entries[key]
	if e == nil {
		e = &histEntry{}
		h.Entries[key] = e
	}
	fn(e)
	e.Updated = time.Now().Unix()
	if src, _, ok := strings.Cut(key, "|"); ok && (e.LocalMs > 0 || e.RemoteMs > 0) {
		h.BySource[src] = &histEntry{LocalMs: e.LocalMs, RemoteMs: e.RemoteMs, OutBytes: e.OutBytes}
	}
	h.dirty = true
}

func (h *history) setDeps(key string, deps []string) {
	h.update(key, func(e *histEntry) {
		e.Deps = e.Deps[:0]
		for _, d := range deps {
			idx, ok := h.pathIdx[d]
			if !ok {
				idx = uint32(len(h.Paths))
				h.Paths = append(h.Paths, d)
				h.pathIdx[d] = idx
			}
			e.Deps = append(e.Deps, idx)
		}
	})
}

func (h *history) setPCHCmd(out string, c pchCmd) {
	h.mu.Lock()
	h.PCHCmds[out] = c
	h.dirty = true
	h.mu.Unlock()
}

func (h *history) pchCmd(out string) (pchCmd, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.PCHCmds[out]
	return c, ok
}

func ewma(old, sample, alpha float64) float64 {
	if old <= 0 {
		return sample
	}
	return old*(1-alpha) + sample*alpha
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".dbuild-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

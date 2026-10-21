package main

import (
	"encoding/gob"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Daemon <-> helper protocol: a single gob stream inside a single zstd stream in each
// direction. The zstd window is shared across messages, so redundancy between objects
// (debug info for the same types, identical COMDATs) compresses away. Every message is
// flushed immediately so results are never held back waiting for more data.

type Hello struct {
	Version  string
	ClientID string
	TopLinks map[string]string // top-level symlinks on the client (macOS: tmp -> private/tmp)
}

type HelloAck struct {
	Version string
	Sandbox bool // helper runs compiles in a namespace where client paths exist verbatim
	Slots   int
	Index   map[string]string // client path -> content hash already mirrored on the helper
}

type PutFile struct {
	Path string // client absolute path
	Hash string
	Data []byte
	Link string // if set: make Path a symlink to this relative target (a case alias); Data is empty
}

type RemoteJob struct {
	ID        uint64
	Replicate bool     // build the output on the helper and keep it there (PCH); send nothing back
	Compiler  string   // client absolute compiler path
	ToolRoot  string   // client toolchain root (parent of bin/)
	Ident     string   // normalized compiler identity
	Argv      []string // compiler args (without argv[0]), client paths
	Cwd       string
	ObjArg    string
	ObjAbs    string
	DepArg    string // "" if the command has no -MF
	DepAbs    string
	WaitFor   []string // client paths of replicated PCH files this job needs
	CL        bool     // clang-cl driver mode
	PCHOut    string   // PCH this job produces (replicate jobs)
}

type Result struct {
	ID         uint64
	Exit       int
	Err        string // infrastructure error (not a compile error)
	Stdout     []byte
	Stderr     []byte
	Outputs    map[string][]byte // client absolute path -> content
	DepHashes  map[string]string // client path -> hash of the mirrored file the compile actually read
	DurationMs int64
}

type Msg struct {
	Hello    *Hello
	HelloAck *HelloAck
	File     *PutFile
	Job      *RemoteJob
	Result   *Result
}

type wireWriter struct {
	mu  sync.Mutex
	zw  *zstd.Encoder
	enc *gob.Encoder
}

func newWireWriter(w io.Writer) *wireWriter {
	zw, _ := zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithWindowSize(64<<20),
		zstd.WithEncoderConcurrency(1))
	return &wireWriter{zw: zw, enc: gob.NewEncoder(zw)}
}

func (w *wireWriter) send(m *Msg) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(m); err != nil {
		return err
	}
	return w.zw.Flush()
}

type wireReader struct {
	zr  *zstd.Decoder
	dec *gob.Decoder
}

func newWireReader(r io.Reader) (*wireReader, error) {
	zr, err := zstd.NewReader(r,
		zstd.WithDecoderMaxWindow(1<<30),
		zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &wireReader{zr: zr, dec: gob.NewDecoder(zr)}, nil
}

func (r *wireReader) recv() (*Msg, error) {
	m := &Msg{}
	if err := r.dec.Decode(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *wireReader) close() { r.zr.Close() }

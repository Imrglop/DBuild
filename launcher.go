package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Launcher <-> daemon IPC: newline-delimited JSON over a unix socket.

type ipcRequest struct {
	Kind    string   `json:"kind"` // compile | done | status | stop
	Version string   `json:"version,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Exit    int      `json:"exit,omitempty"`
	Ms      int64    `json:"ms,omitempty"`
}

type ipcResponse struct {
	Kind   string `json:"kind"` // local | result | ack | status
	Exit   int    `json:"exit,omitempty"`
	Stdout []byte `json:"stdout,omitempty"`
	Stderr []byte `json:"stderr,omitempty"`
	Text   string `json:"text,omitempty"`
}

func runCompilerLocally(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
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
	fmt.Fprintf(os.Stderr, "dbuild: %v\n", err)
	return 127
}

func dialDaemon(spawn bool) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", socketPath(), time.Second)
	if err == nil || !spawn {
		return conn, err
	}
	if err := spawnDaemon(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
		if conn, err = net.DialTimeout("unix", socketPath(), time.Second); err == nil {
			return conn, nil
		}
	}
	return nil, err
}

func spawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(cacheDirFile("daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func runLauncher(argv []string) int {
	if os.Getenv("DBUILD_DISABLE") == "1" {
		return runCompilerLocally(argv)
	}
	cwd, _ := os.Getwd()
	conn, err := dialDaemon(true)
	if err != nil {
		return runCompilerLocally(argv)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := enc.Encode(ipcRequest{Kind: "compile", Version: version, Argv: argv, Cwd: cwd}); err != nil {
		return runCompilerLocally(argv)
	}
	var resp ipcResponse
	if err := dec.Decode(&resp); err != nil {
		return runCompilerLocally(argv)
	}
	if resp.Kind == "result" {
		os.Stdout.Write(resp.Stdout)
		os.Stderr.Write(resp.Stderr)
		return resp.Exit
	}
	start := time.Now()
	code := runCompilerLocally(argv)
	if enc.Encode(ipcRequest{Kind: "done", Exit: code, Ms: time.Since(start).Milliseconds()}) == nil {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		dec.Decode(&resp)
	}
	return code
}

func daemonRequest(req ipcRequest) (*ipcResponse, error) {
	conn, err := dialDaemon(false)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp ipcResponse
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func runStatus() int {
	resp, err := daemonRequest(ipcRequest{Kind: "status"})
	if err != nil {
		fmt.Println("daemon not running")
		return 1
	}
	fmt.Print(resp.Text)
	return 0
}

func runStop() int {
	if _, err := daemonRequest(ipcRequest{Kind: "stop"}); err != nil {
		fmt.Println("daemon not running")
		return 1
	}
	fmt.Println("daemon stopping")
	return 0
}

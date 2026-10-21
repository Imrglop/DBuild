package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// runNinjaWrapper runs the real ninja with a high -j. IDEs such as Android Studio run ninja
// without -j (so ninja picks ncpu+2), and some scripts hardcode a small one; both leave helpers
// idle while dbuild's daemon does the real throttling. A caller's -j is kept unless the repo's
// config sets override_jobs.
func runNinjaWrapper(args []string) int {
	real := findRealNinja()
	if real == "" {
		fmt.Fprintln(os.Stderr, "dbuild-ninja: no ninja found (set [scheduler] ninja in the dbuild config)")
		return 127
	}
	cfg := newConfigStore().forRepo(findBuildInfo(ninjaBuildDir(args)).RepoRoot)
	jobs := 200
	if n := cfg.Scheduler.NinjaJobs; n != nil && *n > 0 {
		jobs = *n
	}
	override := cfg.Scheduler.OverrideJobs != nil && *cfg.Scheduler.OverrideJobs

	var rest []string
	hasJ, tool := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t":
			tool = true
		case a == "-j" && i+1 < len(args):
			hasJ = true
			if override {
				i++
				continue
			}
			rest = append(rest, a, args[i+1])
			i++
			continue
		case strings.HasPrefix(a, "-j") && len(a) > 2:
			hasJ = true
			if override {
				continue
			}
		}
		rest = append(rest, a)
	}
	argv := []string{real}
	if !tool && (!hasJ || override) {
		argv = append(argv, "-j", strconv.Itoa(jobs))
	}
	argv = append(argv, rest...)
	if err := syscall.Exec(real, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "dbuild-ninja: exec %s: %v\n", real, err)
		return 127
	}
	return 0
}

// ninjaBuildDir is the directory ninja will run in (-C dir, else cwd).
func ninjaBuildDir(args []string) string {
	cwd, _ := os.Getwd()
	for i, a := range args {
		if a == "-C" && i+1 < len(args) {
			return absPath(cwd, args[i+1])
		}
		if strings.HasPrefix(a, "-C") && len(a) > 2 {
			return absPath(cwd, a[2:])
		}
	}
	return cwd
}

func findRealNinja() string {
	if p := os.Getenv("DBUILD_NINJA"); p != "" {
		return p
	}
	if p := newConfigStore().global().Scheduler.Ninja; p != nil && *p != "" {
		return *p
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	candidates := filepath.SplitList(os.Getenv("PATH"))
	candidates = append(candidates, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")
	sdkCmake, _ := filepath.Glob(filepath.Join(homeDir(), "Library/Android/sdk/cmake/*/bin"))
	sdkCmake2, _ := filepath.Glob(filepath.Join(homeDir(), "Android/Sdk/cmake/*/bin"))
	candidates = append(candidates, sdkCmake...)
	candidates = append(candidates, sdkCmake2...)
	for _, dir := range candidates {
		p := filepath.Join(dir, "ninja")
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		if r, _ := filepath.EvalSymlinks(p); r == self {
			continue
		}
		return p
	}
	return ""
}

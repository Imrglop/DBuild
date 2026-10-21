package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	ModeDistribute = "distribute"
	ModeLocal      = "local"
)

type SchedulerConfig struct {
	LocalSlots     *int     `toml:"local_slots"`
	RemoteMarginMs *int     `toml:"remote_margin_ms"`
	NinjaJobs      *int     `toml:"ninja_jobs"`    // -j added by the dbuild-ninja wrapper
	OverrideJobs   *bool    `toml:"override_jobs"` // wrapper replaces a caller's -j (e.g. scripts hardcoding --parallel 8)
	Ninja          *string  `toml:"ninja"`         // real ninja for the wrapper (default: search)
	LocalSources   []string `toml:"local_sources"` // source globs (relative to the repo root) always compiled locally
}

type HelperConfig struct {
	Name         string   `toml:"name"`
	SSH          string   `toml:"ssh"`
	SSHOptions   []string `toml:"ssh_options"`
	Slots        int      `toml:"slots"`
	Speed        float64  `toml:"speed"`
	DownloadMbps float64  `toml:"download_mbps"`
	UploadMbps   float64  `toml:"upload_mbps"`
	Toolchains   []string `toml:"toolchains"`
	Enabled      *bool    `toml:"enabled"`
}

type PolicyConfig struct {
	Mode          *string `toml:"mode"`
	ModeFullDebug *string `toml:"mode_full_debug"`
}

type Config struct {
	Scheduler SchedulerConfig         `toml:"scheduler"`
	Helpers   []HelperConfig          `toml:"helpers"`
	Policy    map[string]PolicyConfig `toml:"policy"`
}

const defaultGlobalConfig = `# dbuild global config.
# Per-repo overrides go in <repo>/` + repoConfigName + ` (create with ` + "`dbuild repo-init`" + `);
# it accepts the same [scheduler] and [policy.*] tables and is merged over this file.

[scheduler]
# local_slots = 10          # default: number of CPUs
# remote_margin_ms = 150    # remote must beat local by this much to be used
# ninja_jobs = 200          # -j the dbuild-ninja wrapper adds (IDEs like Android Studio pass none)
# ninja = "/opt/homebrew/bin/ninja"  # real ninja behind the wrapper (default: searched)
# override_jobs = false     # wrapper replaces a caller's -j too (useful per repo, for scripts with --parallel N)

[[helpers]]
name = "fedora-2"
ssh = "fedora-2"            # ssh destination; the helper binary is deployed automatically
# slots = 24                # default: helper CPU count
speed = 1.15                # initial guess: helper per-job time / local per-job time (learned)
download_mbps = 18          # initial guess helper -> this machine (learned)
upload_mbps = 18
# toolchains = ["/opt/android-ndk-*/toolchains/llvm/prebuilt/*"]  # extra helper toolchain roots

# Policy is chosen by CMAKE_BUILD_TYPE (exact name), falling back to [policy.default].
# mode            = "distribute" | "local"
# mode_full_debug = mode used when objects carry full DWARF (-g/-g2/-g3)
[policy.default]
mode = "distribute"

[policy.Debug]
mode_full_debug = "local"
`

const defaultRepoConfig = `# Untracked dbuild overrides for this repo (merged over ~/.config/dbuild/config.toml).

# [scheduler]
# local_slots = 8
# ninja_jobs = 200
# override_jobs = true      # ignore -j from scripts/presets when building through dbuild-ninja
# local_sources = ["src/assets/*.cpp", "**/generated/FolderData.cpp"]  # never distribute these

# [policy.default]
# mode = "distribute"

# [policy.Debug]
# mode_full_debug = "local"

# [policy.Release]
# mode = "distribute"
`

func loadConfigFile(path string) (*Config, error) {
	c := &Config{}
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, err
	}
	return c, nil
}

// merge applies o's scheduler/policy settings over c. Helpers are daemon-wide and only
// come from the global file.
func (c *Config) merge(o *Config) *Config {
	out := *c
	out.Policy = map[string]PolicyConfig{}
	for k, v := range c.Policy {
		out.Policy[k] = v
	}
	if o.Scheduler.LocalSlots != nil {
		out.Scheduler.LocalSlots = o.Scheduler.LocalSlots
	}
	if o.Scheduler.RemoteMarginMs != nil {
		out.Scheduler.RemoteMarginMs = o.Scheduler.RemoteMarginMs
	}
	if o.Scheduler.NinjaJobs != nil {
		out.Scheduler.NinjaJobs = o.Scheduler.NinjaJobs
	}
	if o.Scheduler.OverrideJobs != nil {
		out.Scheduler.OverrideJobs = o.Scheduler.OverrideJobs
	}
	if o.Scheduler.LocalSources != nil {
		out.Scheduler.LocalSources = o.Scheduler.LocalSources
	}
	for k, v := range o.Policy {
		cur := out.Policy[k]
		if v.Mode != nil {
			cur.Mode = v.Mode
		}
		if v.ModeFullDebug != nil {
			cur.ModeFullDebug = v.ModeFullDebug
		}
		out.Policy[k] = cur
	}
	return &out
}

// forcedLocal reports whether src matches one of the repo's local_sources globs. Globs are
// relative to the repo root (or absolute); * and ? stay within a path component, ** spans any.
func (c *Config) forcedLocal(repoRoot, src string) bool {
	rel := src
	if repoRoot != "" {
		if r, err := filepath.Rel(repoRoot, src); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	for _, g := range c.Scheduler.LocalSources {
		target := rel
		if filepath.IsAbs(g) {
			target = src
		}
		if globRegexp(g).MatchString(target) {
			return true
		}
	}
	return false
}

func globRegexp(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func (c *Config) policyFor(buildType string) (PolicyConfig, PolicyConfig) {
	def := c.Policy["default"]
	if p, ok := c.Policy[buildType]; ok {
		return p, def
	}
	for k, p := range c.Policy {
		if strings.EqualFold(k, buildType) {
			return p, def
		}
	}
	return PolicyConfig{}, def
}

func (c *Config) modeFor(buildType string, dbg DebugLevel) string {
	bt, def := c.policyFor(buildType)
	pick := func(vals ...*string) string {
		for _, v := range vals {
			if v != nil {
				return *v
			}
		}
		return ModeDistribute
	}
	if dbg == DebugFull {
		return pick(bt.ModeFullDebug, def.ModeFullDebug, bt.Mode, def.Mode)
	}
	return pick(bt.Mode, def.Mode)
}

// configStore caches parsed config files and reloads them when they change on disk.
type configStore struct {
	mu    sync.Mutex
	files map[string]*cachedConfig
}

type cachedConfig struct {
	mtime   time.Time
	checked time.Time
	cfg     *Config
	err     error
}

func newConfigStore() *configStore { return &configStore{files: map[string]*cachedConfig{}} }

func (s *configStore) get(path string) *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	cc := s.files[path]
	if cc != nil && time.Since(cc.checked) < time.Second {
		return cc.cfg
	}
	st, err := os.Stat(path)
	if err != nil {
		s.files[path] = &cachedConfig{checked: time.Now()}
		return nil
	}
	if cc != nil && st.ModTime().Equal(cc.mtime) {
		cc.checked = time.Now()
		return cc.cfg
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		logf("config %s: %v", path, err)
	}
	s.files[path] = &cachedConfig{mtime: st.ModTime(), checked: time.Now(), cfg: cfg, err: err}
	return cfg
}

func (s *configStore) global() *Config {
	if c := s.get(globalConfigPath()); c != nil {
		return c
	}
	return &Config{Policy: map[string]PolicyConfig{}}
}

func (s *configStore) forRepo(repoRoot string) *Config {
	g := s.global()
	if repoRoot == "" {
		return g
	}
	if r := s.get(filepath.Join(repoRoot, repoConfigName)); r != nil {
		return g.merge(r)
	}
	return g
}

// BuildInfo describes the CMake build dir a compile runs in.
type BuildInfo struct {
	BuildType string
	RepoRoot  string
	cacheFile string
	mtime     time.Time
}

func findBuildInfo(cwd string) *BuildInfo {
	dir := cwd
	for i := 0; i < 8; i++ {
		cache := filepath.Join(dir, "CMakeCache.txt")
		if st, err := os.Stat(cache); err == nil {
			bi := &BuildInfo{cacheFile: cache, mtime: st.ModTime()}
			home := ""
			if f, err := os.Open(cache); err == nil {
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				for sc.Scan() {
					line := sc.Text()
					if v, ok := strings.CutPrefix(line, "CMAKE_BUILD_TYPE:"); ok {
						bi.BuildType = v[strings.IndexByte(v, '=')+1:]
					} else if v, ok := strings.CutPrefix(line, "CMAKE_HOME_DIRECTORY:"); ok {
						home = v[strings.IndexByte(v, '=')+1:]
					}
				}
				f.Close()
			}
			if home != "" {
				bi.RepoRoot = findRepoRoot(home)
			}
			return bi
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return &BuildInfo{RepoRoot: findRepoRoot(cwd)}
}

func findRepoRoot(dir string) string {
	d := dir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

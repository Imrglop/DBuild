package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type DebugLevel int

const (
	DebugNone DebugLevel = iota
	DebugLine
	DebugFull
)

func (d DebugLevel) String() string {
	return [...]string{"none", "line", "full"}[d]
}

// CompileInfo is what we understand about one compiler invocation.
type CompileInfo struct {
	Compiler string
	ToolRoot string // parent of the compiler's bin/ dir
	Source   string // absolute
	Lang     string
	ObjArg   string
	ObjAbs   string
	DepArg   string
	DepAbs   string
	Debug    DebugLevel
	Opt      string
	Target   string
	Args     []string // argv[1:] with @response files expanded
	CL       bool     // clang-cl driver mode (MSVC-style flags)
	PCHUses  []string // absolute paths of PCH files passed with -include-pch (or /Yu + /Fp)
	IsPCH    bool     // this command produces a PCH
	PCHOut   string   // the PCH it produces: ObjAbs, or the /Fp path for /Yc (whose obj is separate)
	RemoteOK bool
	Reason   string // why RemoteOK is false
}

// Flags whose value is the following argument.
var sepArgFlags = map[string]bool{
	"-o": true, "-MF": true, "-MT": true, "-MQ": true, "-x": true,
	"-I": true, "-isystem": true, "-iquote": true, "-idirafter": true, "-cxx-isystem": true,
	"-isystem-after": true, "-include": true, "-imacros": true, "-include-pch": true,
	"-Xclang": true, "-Xlinker": true, "-Xassembler": true, "-Xpreprocessor": true,
	"-target": true, "-arch": true, "-isysroot": true, "-D": true, "-U": true,
	"-L": true, "-F": true, "-B": true, "-mllvm": true, "-iprefix": true, "-iwithprefix": true,
	"-iwithprefixbefore": true, "--sysroot": true, "-gcc-toolchain": true, "-resource-dir": true,
	"-ivfsoverlay": true, "-imultilib": true, "-MJ": true, "-z": true, "-u": true,
	"-working-directory": true, "--serialize-diagnostics": true, "-serialize-diagnostics": true,
}

// Flags that make a command produce extra outputs, read inputs we can't see in the depfile,
// or not be a plain compile. Matched as prefixes.
var unsupportedPrefixes = []string{
	"-save-temps", "-ftime-trace", "-gsplit-dwarf", "-MJ", "--serialize-diagnostics",
	"-serialize-diagnostics", "-fmodules", "-fmodule-file", "-fprebuilt-module-path",
	"-ivfsoverlay", "-fcrash-diagnostics", "-working-directory", "-fsyntax-only",
	"-emit-llvm", "-###", "-print-", "--print-", "-fprofile-use", "-fprofile-instr-use",
	"-fprofile-sample-use", "-fsanitize-blacklist", "-fsanitize-ignorelist", "-fembed-bitcode",
	"--analyze", "-fdebug-info-for-profiling",
}

var unsupportedExact = map[string]bool{"-E": true, "-S": true, "-M": true, "-MM": true, "-MMD": true, "-": true}

func absPath(cwd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(cwd, p))
}

func isPCHPath(p string) bool {
	return strings.HasSuffix(p, ".pch") || strings.HasSuffix(p, ".gch")
}

func langOf(src, x string) string {
	switch x {
	case "c":
		return "c"
	case "c++":
		return "c++"
	case "c++-header", "c-header":
		return x
	case "":
	default:
		return "other"
	}
	switch strings.ToLower(filepath.Ext(src)) {
	case ".c":
		return "c"
	case ".cc", ".cpp", ".cxx", ".c++":
		return "c++"
	case ".s", ".asm":
		return "asm"
	}
	if filepath.Ext(src) == ".C" {
		return "c++"
	}
	return "other"
}

func parseCompile(argv []string, cwd string) *CompileInfo {
	if len(argv) == 0 {
		return &CompileInfo{Reason: "empty command", Lang: "other"}
	}
	ci := &CompileInfo{Compiler: argv[0]}
	if !filepath.IsAbs(ci.Compiler) {
		if p, err := exec.LookPath(ci.Compiler); err == nil {
			ci.Compiler, _ = filepath.Abs(p)
		}
	}
	fail := func(r string) *CompileInfo {
		if ci.Reason == "" {
			ci.Reason = r
		}
		ci.RemoteOK = false
		return ci
	}
	base := filepath.Base(ci.Compiler)
	if !strings.Contains(base, "clang") || strings.Contains(base, "scan-deps") || strings.Contains(base, "tidy") {
		return fail("not a clang compiler")
	}
	// Resolve the directory (not the file: clang-cl -> clang, and the name picks the driver
	// mode) so e.g. Homebrew's opt/llvm symlink gives the Cellar root that depfiles name.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(ci.Compiler)); err == nil {
		ci.Compiler = filepath.Join(dir, base)
	}
	if filepath.Base(filepath.Dir(ci.Compiler)) == "bin" {
		ci.ToolRoot = filepath.Dir(filepath.Dir(ci.Compiler))
	}

	args, err := expandResponseFiles(argv[1:], cwd)
	if err != nil {
		return fail("response file: " + err.Error())
	}
	ci.Args = args
	ci.CL = strings.Contains(base, "clang-cl")
	for _, a := range args {
		if strings.HasPrefix(a, "--driver-mode=") {
			ci.CL = a == "--driver-mode=cl"
		}
	}
	var positional []string
	var hasC, hasMD bool
	var xLang string
	if ci.CL {
		positional, hasC, hasMD, xLang = ci.parseCLArgs(cwd, fail)
	} else {
		positional, hasC, hasMD, xLang = ci.parseGNUArgs(cwd, fail)
	}
	if len(positional) == 1 {
		ci.Source = absPath(cwd, positional[0])
	}
	ci.Lang = langOf(ci.Source, xLang)
	if ci.ObjArg != "" {
		ci.ObjAbs = absPath(cwd, ci.ObjArg)
		if !ci.CL && isPCHPath(ci.ObjAbs) && (ci.Lang == "c++-header" || ci.Lang == "c-header") {
			ci.IsPCH = true
		}
	}
	if ci.IsPCH && ci.PCHOut == "" {
		ci.PCHOut = ci.ObjAbs
	}
	if ci.DepArg != "" {
		ci.DepAbs = absPath(cwd, ci.DepArg)
	}

	switch {
	case ci.Reason != "":
		return ci
	case !hasC:
		return fail("not a compile (-c missing)")
	case len(positional) != 1:
		return fail("expected exactly one source file")
	case ci.ObjArg == "":
		return fail("no -o")
	case ci.ToolRoot == "":
		return fail("compiler not in a <root>/bin dir")
	case hasMD != (ci.DepArg != ""):
		return fail("-MD without -MF (or vice versa)")
	}
	switch ci.Lang {
	case "c", "c++":
	case "c++-header", "c-header":
		if !ci.IsPCH {
			return fail("header compile")
		}
	default:
		return fail("language " + ci.Lang)
	}
	ci.RemoteOK = true
	return ci
}

// parseGNUArgs reads a gcc-style clang command line.
func (ci *CompileInfo) parseGNUArgs(cwd string, fail func(string) *CompileInfo) (positional []string, hasC, hasMD bool, xLang string) {
	args := ci.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		if unsupportedExact[a] {
			fail("unsupported flag " + a)
			continue
		}
		for _, p := range unsupportedPrefixes {
			if strings.HasPrefix(a, p) {
				fail("unsupported flag " + a)
			}
		}
		switch {
		case a == "-c":
			hasC = true
		case a == "-MD":
			hasMD = true
		case a == "-o":
			ci.ObjArg = next()
		case a == "-MF":
			ci.DepArg = next()
		case a == "-x":
			xLang = next()
		case a == "-target":
			ci.Target = next()
		case strings.HasPrefix(a, "--target="):
			ci.Target = strings.TrimPrefix(a, "--target=")
		case a == "-include-pch":
			ci.PCHUses = append(ci.PCHUses, absPath(cwd, next()))
		case a == "-include":
			inc := next()
			for _, ext := range []string{".pch", ".gch"} {
				if _, err := os.Stat(absPath(cwd, inc) + ext); err == nil {
					fail("implicit PCH via -include")
				}
			}
		case a == "-Xclang":
			v := next()
			switch v {
			case "-emit-pch":
				ci.IsPCH = true
			case "-include-pch":
				if i+2 < len(args) && args[i+1] == "-Xclang" {
					ci.PCHUses = append(ci.PCHUses, absPath(cwd, args[i+2]))
					i += 2
				}
			}
		case sepArgFlags[a]:
			next()
		case strings.HasPrefix(a, "-O"):
			ci.Opt = a
		case strings.HasPrefix(a, "-g"):
			if d, ok := gnuDebugLevel(a); ok {
				ci.Debug = d
			}
		case !strings.HasPrefix(a, "-"):
			positional = append(positional, a)
		}
	}
	return
}

func gnuDebugLevel(a string) (DebugLevel, bool) {
	switch {
	case a == "-g0" || a == "-ggdb0":
		return DebugNone, true
	case a == "-g1" || a == "-gline-tables-only" || a == "-gmlt" || a == "-gline-directives-only" || a == "-ggdb1":
		return DebugLine, true
	case a == "-g" || a == "-g2" || a == "-g3" || strings.HasPrefix(a, "-ggdb") || a == "-glldb" ||
		a == "-gsce" || a == "-gfull" || strings.HasPrefix(a, "-gdwarf"):
		return DebugFull, true
	}
	return 0, false
}

// cl-mode options (spelled with / or -) that take the next argument when written alone.
var clSepArgFlags = map[string]bool{
	"I": true, "D": true, "U": true, "FI": true, "imsvc": true, "external:I": true, "winsysroot": true,
	"vctoolsdir": true, "winsdkdir": true, "o": true, "Xclang": true, "mllvm": true, "target": true,
	"include": true, "isystem": true, "idirafter": true, "iquote": true, "Tc": true, "Tp": true,
}

// cl-mode options that preprocess only, write extra outputs, or emit something other than an object.
var clUnsupportedExact = map[string]bool{"E": true, "EP": true, "P": true, "Zs": true, "Yd": true}
var clUnsupportedPrefixes = []string{"FA", "Fa", "Fi", "Fe", "Fm", "Fr", "FR", "sourceDependencies", "Zg", "Y-", "clr", "analyze"}

// parseCLArgs reads a clang-cl command line. Options start with / or -; everything after
// "--" is an input (CMake always uses it, since /Users/... would otherwise parse as /U).
// -clang:<opt> / /clang:<opt> pass gcc-style options (CMake's depfile flags) through.
func (ci *CompileInfo) parseCLArgs(cwd string, fail func(string) *CompileInfo) (positional []string, hasC, hasMD bool, lang string) {
	args := ci.Args
	var pchFile string
	var yc, yu bool
	clangNext := "" // gcc-style flag waiting for its value from the next -clang: arg
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if g, ok := strings.CutPrefix(a, "-clang:"); ok || strings.HasPrefix(a, "/clang:") {
			if !ok {
				g = a[len("/clang:"):]
			}
			if clangNext != "" {
				if clangNext == "-MF" {
					ci.DepArg = g
				}
				clangNext = ""
				continue
			}
			if unsupportedExact[g] {
				fail("unsupported flag " + a)
				continue
			}
			for _, p := range unsupportedPrefixes {
				if strings.HasPrefix(g, p) {
					fail("unsupported flag " + a)
				}
			}
			switch {
			case g == "-MD":
				hasMD = true
			case g == "-MF" || g == "-MT" || g == "-MQ":
				clangNext = g
			case strings.HasPrefix(g, "-MF"):
				ci.DepArg = g[3:]
			case strings.HasPrefix(g, "-O"):
				ci.Opt = g
			case strings.HasPrefix(g, "-g"):
				if d, ok := gnuDebugLevel(g); ok {
					ci.Debug = d
				}
			}
			continue
		}
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "/") {
			positional = append(positional, a)
			continue
		}
		if strings.HasPrefix(a, "-") {
			for _, p := range unsupportedPrefixes {
				if strings.HasPrefix(a, p) {
					fail("unsupported flag " + a)
				}
			}
		}
		switch {
		case a == "-target":
			ci.Target = next()
			continue
		case strings.HasPrefix(a, "--target="):
			ci.Target = strings.TrimPrefix(a, "--target=")
			continue
		case strings.HasPrefix(a, "--"):
			continue
		case a == "-Xclang":
			switch next() {
			case "-emit-pch":
				ci.IsPCH = true
			case "-include-pch":
				if i+2 < len(args) && args[i+1] == "-Xclang" {
					ci.PCHUses = append(ci.PCHUses, absPath(cwd, args[i+2]))
					i += 2
				}
			}
			continue
		case strings.HasPrefix(a, "-g"):
			if d, ok := gnuDebugLevel(a); ok {
				ci.Debug = d
			}
			continue
		}
		opt := a[1:]
		if clUnsupportedExact[opt] {
			fail("unsupported flag " + a)
			continue
		}
		for _, p := range clUnsupportedPrefixes {
			if strings.HasPrefix(opt, p) {
				fail("unsupported flag " + a)
			}
		}
		switch {
		case opt == "c":
			hasC = true
		case opt == "o":
			ci.ObjArg = next()
		case opt == "Fo:":
			ci.ObjArg = next()
		case strings.HasPrefix(opt, "Fo"):
			ci.ObjArg = strings.TrimPrefix(opt[2:], ":")
		case opt == "Fp:":
			pchFile = next()
		case strings.HasPrefix(opt, "Fp"):
			pchFile = strings.TrimPrefix(opt[2:], ":")
		case strings.HasPrefix(opt, "Yc"):
			yc = true
		case strings.HasPrefix(opt, "Yu"):
			yu = true
		case opt == "TP":
			lang = "c++"
		case opt == "TC":
			lang = "c"
		case opt == "Tp" || opt == "Tc":
			positional = append(positional, next())
			lang = map[string]string{"Tp": "c++", "Tc": "c"}[opt]
		case strings.HasPrefix(opt, "Tp") || strings.HasPrefix(opt, "Tc"):
			positional = append(positional, opt[2:])
			lang = map[string]string{"Tp": "c++", "Tc": "c"}[opt[:2]]
		case clSepArgFlags[opt]:
			next()
		case opt == "Zi" || opt == "Z7" || opt == "ZI":
			ci.Debug = DebugFull
		case opt == "O1" || opt == "O2" || opt == "Od" || opt == "Ox" || opt == "Os" || opt == "Ot":
			ci.Opt = a
		}
	}
	switch {
	case (yc || yu) && pchFile == "":
		fail("/Yc or /Yu without /Fp")
	case yc && yu:
		fail("/Yc together with /Yu")
	case yc:
		ci.IsPCH = true
		ci.PCHOut = absPath(cwd, pchFile)
	case yu:
		ci.PCHUses = append(ci.PCHUses, absPath(cwd, pchFile))
	}
	return
}

// historyKey identifies "the same compile" across build dirs and clean builds. The object
// path relative to the build dir (CMakeFiles/<target>.dir/...) distinguishes one source
// compiled by several targets with different flags.
func (ci *CompileInfo) historyKey() string {
	obj := ci.ObjArg
	if filepath.IsAbs(obj) {
		obj = filepath.Base(obj)
	}
	return ci.Source + "|" + ci.Target + "|" + ci.Opt + "|" + ci.Debug.String() + "|" + obj
}

// parseDepfile returns the prerequisites of the first rule in a Makefile-style depfile.
func parseDepfile(content string) []string {
	content = strings.ReplaceAll(content, "\\\r\n", " ")
	content = strings.ReplaceAll(content, "\\\n", " ")
	if nl := strings.IndexByte(content, '\n'); nl >= 0 {
		content = content[:nl]
	}
	idx := strings.Index(content, ": ")
	if idx < 0 {
		if strings.HasSuffix(content, ":") {
			return nil
		}
		return nil
	}
	rest := content[idx+2:]
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '\\' && i+1 < len(rest) && (rest[i+1] == ' ' || rest[i+1] == '#'):
			cur.WriteByte(rest[i+1])
			i++
		case c == '$' && i+1 < len(rest) && rest[i+1] == '$':
			cur.WriteByte('$')
			i++
		case c == ' ' || c == '\t' || c == '\r':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

var identFlagRe = regexp.MustCompile(`[+-](pgo|bolt|lto|mlgo|thinlto)\b,?\s*`)

// Upstream release builds name their source commit; Homebrew builds of the same tag don't.
var identUpstreamRe = regexp.MustCompile(`^(clang version \d+\.\d+\.\d+) \(https://github\.com/llvm/llvm-project(\.git)? [0-9a-f]+\)$`)

// normalizeIdent turns `clang --version` output into an identity that is equal for the same
// compiler release built for different hosts (e.g. NDK darwin vs linux builds, which differ
// only in how the compiler binary itself was optimized, or Homebrew's LLVM and the upstream
// Linux release of the same version).
func normalizeIdent(versionOutput string) string {
	line := strings.TrimSpace(strings.SplitN(versionOutput, "\n", 2)[0])
	line = strings.TrimSpace(identFlagRe.ReplaceAllString(line, ""))
	line = strings.TrimPrefix(line, "Homebrew ")
	return identUpstreamRe.ReplaceAllString(line, "$1")
}

// pathMapper rewrites client paths into a helper's mirror / toolchain and back.
type pathMapper struct {
	mirror     string // helper dir that mirrors the client's filesystem root
	clientRoot string // client toolchain root
	helperRoot string // helper toolchain root
	links      map[string]string
}

func underDir(p, dir string) bool {
	return dir != "" && (p == dir || strings.HasPrefix(p, dir+"/"))
}

func (m pathMapper) path(p string) string {
	if underDir(p, m.clientRoot) {
		return m.helperRoot + p[len(m.clientRoot):]
	}
	return m.mirror + canonTop(p, m.links)
}

var joinedPathFlags = []string{"-I", "-L", "-F", "-B", "-isystem", "-iquote", "-idirafter", "-include", "-imacros", "-o", "-MF"}

var prefixMapFlags = []string{"-ffile-prefix-map=", "-fdebug-prefix-map=", "-fmacro-prefix-map=", "-fcoverage-prefix-map=", "-fprofile-prefix-map="}

func (m pathMapper) arg(a string) string {
	if strings.HasPrefix(a, "/") {
		return m.path(a)
	}
	if !strings.HasPrefix(a, "-") {
		return a
	}
	for _, f := range prefixMapFlags {
		if strings.HasPrefix(a, f) {
			// Map only the OLD side; NEW is what ends up in the client's output.
			v := a[len(f):]
			if eq := strings.IndexByte(v, '='); eq > 0 && strings.HasPrefix(v, "/") {
				return f + m.path(v[:eq]) + v[eq:]
			}
			return a
		}
	}
	for _, f := range joinedPathFlags {
		if strings.HasPrefix(a, f+"/") {
			return f + m.path(a[len(f):])
		}
	}
	if i := strings.Index(a, "=/"); i > 0 {
		return a[:i+1] + m.path(a[i+1:])
	}
	return a
}

func (m pathMapper) unmap(s string) string {
	s = strings.ReplaceAll(s, m.mirror+"/", "/")
	if m.helperRoot != "" && m.helperRoot != m.clientRoot {
		s = strings.ReplaceAll(s, m.helperRoot, m.clientRoot)
	}
	return s
}

// expandResponseFiles inlines @file arguments (GNU tokenization, as clang does on Unix).
func expandResponseFiles(args []string, cwd string) ([]string, error) {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "@") || len(a) == 1 {
			out = append(out, a)
			continue
		}
		data, err := os.ReadFile(absPath(cwd, a[1:]))
		if err != nil {
			return nil, err
		}
		out = append(out, tokenizeGNU(string(data))...)
	}
	return out, nil
}

func tokenizeGNU(s string) []string {
	var out []string
	var cur strings.Builder
	inTok := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
			inTok = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inTok = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}

// scanArgs builds a dependency-only (-E -M) variant of a compile command.
func scanArgs(ci *CompileInfo, depOut string) []string {
	if ci.CL {
		return scanArgsCL(ci.Args, depOut)
	}
	args := ci.Args
	out := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-c", "-MD", "-MMD", "-MP":
			continue
		case "-o", "-MF", "-MT", "-MQ":
			i++
			continue
		case "-Xclang":
			if i+1 < len(args) && args[i+1] == "-emit-pch" {
				i++
				continue
			}
		}
		out = append(out, a)
	}
	return append(out, "-E", "-M", "-MF", depOut, "-o", os.DevNull)
}

// scanArgsCL is scanArgs for clang-cl. MSVC-style PCH flags are dropped so the scan lists the
// headers behind the PCH (a replicated PCH is checked against them on the helper).
func scanArgsCL(args []string, depOut string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, "-clang:-M", "-clang:-MF"+depOut)
			return append(out, args[i:]...)
		}
		g, isClang := strings.CutPrefix(a, "-clang:")
		if !isClang {
			g, isClang = strings.CutPrefix(a, "/clang:")
		}
		if isClang {
			switch {
			case g == "-MD" || g == "-MMD" || g == "-MP":
				continue
			case g == "-MF" || g == "-MT" || g == "-MQ":
				i++
				continue
			case strings.HasPrefix(g, "-MF") || strings.HasPrefix(g, "-MT") || strings.HasPrefix(g, "-MQ"):
				continue
			}
		} else if len(a) > 1 && (a[0] == '/' || a[0] == '-') {
			opt := a[1:]
			switch {
			case opt == "c":
				continue
			case opt == "o" || opt == "Fo:" || opt == "Fp:":
				i++
				continue
			case strings.HasPrefix(opt, "Fo") || strings.HasPrefix(opt, "Fd") || strings.HasPrefix(opt, "Fp") ||
				strings.HasPrefix(opt, "Yc") || strings.HasPrefix(opt, "Yu"):
				continue
			}
		}
		out = append(out, a)
	}
	return append(out, "-clang:-M", "-clang:-MF"+depOut)
}

// withDepfileFlags adds flags making the compiler write a depfile to dep, before any "--"
// in cl mode (arguments after it are inputs).
func withDepfileFlags(args []string, cl bool, dep string) []string {
	if !cl {
		return append(append([]string{}, args...), "-MD", "-MF", dep)
	}
	extra := []string{"-clang:-MD", "-clang:-MF" + dep}
	out := make([]string, 0, len(args)+2)
	for i, a := range args {
		if a == "--" {
			out = append(out, extra...)
			return append(out, args[i:]...)
		}
		out = append(out, a)
	}
	return append(out, extra...)
}

// rootLinks lists top-level symlinks of "/" (e.g. macOS /tmp -> private/tmp).
func rootLinks() map[string]string {
	out := map[string]string{}
	entries, _ := os.ReadDir("/")
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			if t, err := os.Readlink("/" + e.Name()); err == nil {
				out[e.Name()] = strings.TrimPrefix(t, "/")
			}
		}
	}
	return out
}

// canonTop resolves a top-level symlink at the start of an absolute path, so files reached
// through /tmp and /private/tmp are mirrored once.
func canonTop(p string, links map[string]string) string {
	if len(links) == 0 || !strings.HasPrefix(p, "/") {
		return p
	}
	rest := p[1:]
	top, tail, _ := strings.Cut(rest, "/")
	if t, ok := links[top]; ok {
		if tail == "" {
			return "/" + t
		}
		return "/" + t + "/" + tail
	}
	return p
}

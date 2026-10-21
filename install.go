package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func runInit() int {
	p := globalConfigPath()
	if _, err := os.Stat(p); err == nil {
		fmt.Println("config already exists:", p)
		return 0
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(defaultGlobalConfig), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote", p)
	return 0
}

func runRepoInit() int {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "not in a git repo")
		return 1
	}
	root := strings.TrimSpace(string(out))
	p := filepath.Join(root, repoConfigName)
	if _, err := os.Stat(p); err != nil {
		if err := os.WriteFile(p, []byte(defaultRepoConfig), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("wrote", p)
	}
	out, err = exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return 1
	}
	exclude := strings.TrimSpace(string(out))
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(root, exclude)
	}
	data, _ := os.ReadFile(exclude)
	line := "/" + repoConfigName
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return 0
		}
	}
	os.MkdirAll(filepath.Dir(exclude), 0o755)
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		f.WriteString("\n")
	}
	f.WriteString(line + "\n")
	fmt.Println("added", line, "to", exclude)
	return 0
}

const launchAgentLabel = "dev.dbuild.env"

func launchAgentPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func envdPath() string {
	return filepath.Join(homeDir(), ".config", "environment.d", "dbuild.conf")
}

// install sets CMAKE_{C,CXX}_COMPILER_LAUNCHER in the GUI/session environment. CMake reads
// these on the first configure of a build dir, so IDEs pick dbuild up without any
// per-project settings. Existing build dirs need a reconfigure (or -D...LAUNCHER=).
func gradleInitPath() string {
	return filepath.Join(homeDir(), ".gradle", "init.d", "dbuild.gradle")
}

const gradleInitTemplate = `// Written by ` + "`dbuild install gradle`" + `; remove with ` + "`dbuild uninstall`" + `.
// Routes Android Gradle Plugin CMake builds (Android Studio and ./gradlew) through dbuild:
// compiler launcher, the dbuild-ninja wrapper (AGP runs ninja without -j) and the CMake include
// that lifts project compile-pool caps. Disable for a build with -Pdbuild.enabled=false.
def dbuildExe = '%s'
def dbuildNinja = '%s'
def dbuildInclude = '%s'

allprojects {
    ['com.android.application', 'com.android.library'].each { id ->
        pluginManager.withPlugin(id) {
            if (!new File(dbuildExe).canExecute() || findProperty('dbuild.enabled') == 'false') {
                return
            }
            android.defaultConfig.externalNativeBuild.cmake.arguments(
                "-DCMAKE_C_COMPILER_LAUNCHER=${dbuildExe}",
                "-DCMAKE_CXX_COMPILER_LAUNCHER=${dbuildExe}",
                "-DCMAKE_MAKE_PROGRAM=${dbuildNinja}",
                "-DCMAKE_PROJECT_INCLUDE=${dbuildInclude}")
        }
    }
}
`

func installGradle(exe string) int {
	dir := filepath.Dir(exe)
	include := filepath.Join(filepath.Dir(dir), "cmake", "dbuild.cmake")
	if _, err := os.Stat(include); err != nil {
		fmt.Fprintln(os.Stderr, "missing", include)
		return 1
	}
	script := fmt.Sprintf(gradleInitTemplate, exe, filepath.Join(dir, "dbuild-ninja"), include)
	os.MkdirAll(filepath.Dir(gradleInitPath()), 0o755)
	if err := os.WriteFile(gradleInitPath(), []byte(script), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote", gradleInitPath(), "(Android Studio: restart or stop the Gradle daemon to pick it up)")
	return 0
}

func runInstall() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	exe, _ = filepath.EvalSymlinks(exe)
	what := ""
	if len(os.Args) > 2 {
		what = os.Args[2]
	}
	if what == "gradle" {
		return installGradle(exe)
	}
	if what == "" {
		if rc := installGradle(exe); rc != 0 {
			return rc
		}
	}
	vars := []string{"CMAKE_C_COMPILER_LAUNCHER", "CMAKE_CXX_COMPILER_LAUNCHER"}
	switch runtime.GOOS {
	case "darwin":
		var cmds []string
		for _, v := range vars {
			cmds = append(cmds, fmt.Sprintf("launchctl setenv %s %s", v, exe))
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>/bin/sh</string><string>-c</string><string>%s</string></array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
`, launchAgentLabel, strings.Join(cmds, "; "))
		os.MkdirAll(filepath.Dir(launchAgentPath()), 0o755)
		if err := os.WriteFile(launchAgentPath(), []byte(plist), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, v := range vars {
			exec.Command("launchctl", "setenv", v, exe).Run()
		}
		fmt.Println("wrote", launchAgentPath(), "and set the launcher for GUI apps (restart CLion/Android Studio).")
	default:
		var b strings.Builder
		for _, v := range vars {
			fmt.Fprintf(&b, "%s=%s\n", v, exe)
		}
		os.MkdirAll(filepath.Dir(envdPath()), 0o755)
		if err := os.WriteFile(envdPath(), []byte(b.String()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("wrote", envdPath(), "(applies after re-login).")
	}
	fmt.Println("For shells, add:")
	for _, v := range vars {
		fmt.Printf("  export %s=%s\n", v, exe)
	}
	fmt.Println("Applies to newly configured build dirs. Use a high job count (e.g. -j 40) so ninja hands dbuild enough work to spread.")
	return 0
}

func runUninstall() int {
	os.Remove(gradleInitPath())
	switch runtime.GOOS {
	case "darwin":
		os.Remove(launchAgentPath())
		exec.Command("launchctl", "unsetenv", "CMAKE_C_COMPILER_LAUNCHER").Run()
		exec.Command("launchctl", "unsetenv", "CMAKE_CXX_COMPILER_LAUNCHER").Run()
	default:
		os.Remove(envdPath())
	}
	fmt.Println("removed launcher environment")
	return 0
}

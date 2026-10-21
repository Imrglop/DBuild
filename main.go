// dbuild: distributed compile launcher for CMake/Ninja C/C++ builds.
//
// Usage as a compiler launcher (what CMake runs):
//
//	dbuild /path/to/clang++ <args...>
//
// Subcommands:
//
//	dbuild daemon            run the local scheduler daemon (auto-started by the launcher)
//	dbuild serve             run as a helper over stdin/stdout (started by the daemon via ssh)
//	dbuild status            print daemon state
//	dbuild stop              stop the daemon
//	dbuild init              write a default global config if none exists
//	dbuild repo-init         create an untracked per-repo override file in the current repo
//	dbuild install [env|gradle]  make CMake (env) and/or Android Gradle builds (gradle) use dbuild
//	dbuild uninstall         undo install
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

var version = "dev"

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func cacheDir() string {
	if d := os.Getenv("DBUILD_CACHE_DIR"); d != "" {
		return d
	}
	return filepath.Join(homeDir(), ".cache", "dbuild")
}

func socketPath() string { return filepath.Join(cacheDir(), "daemon.sock") }

func globalConfigPath() string {
	if p := os.Getenv("DBUILD_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".config", "dbuild", "config.toml")
}

const repoConfigName = ".dbuild.local.toml"

func main() {
	// Invoked as the CMAKE_MAKE_PROGRAM wrapper (bin/dbuild-ninja -> dbuild).
	if filepath.Base(os.Args[0]) == "dbuild-ninja" {
		os.Exit(runNinjaWrapper(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dbuild <compiler> <args...> | daemon | serve | status | stop | init | repo-init | install | uninstall | version")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		runDaemon()
	case "serve":
		runServe(os.Args[2:])
	case "status":
		os.Exit(runStatus())
	case "stop":
		os.Exit(runStop())
	case "init":
		os.Exit(runInit())
	case "repo-init":
		os.Exit(runRepoInit())
	case "install":
		os.Exit(runInstall())
	case "uninstall":
		os.Exit(runUninstall())
	case "version", "--version":
		fmt.Println(version)
	default:
		os.Exit(runLauncher(os.Args[1:]))
	}
}

package web

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// uniqueZipName returns a filename that hasn't been used yet in the current
// zip archive. If the requested name collides, we append "-2", "-3", … before
// the extension. Empty input is replaced with "file" so the zip can still
// extract on case-insensitive filesystems.
func uniqueZipName(name string, used map[string]int) string {
	clean := strings.TrimSpace(filepath.Base(name))
	if clean == "" || clean == "." || clean == ".." {
		clean = "file"
	}
	if _, taken := used[clean]; !taken {
		used[clean] = 1
		return clean
	}
	ext := filepath.Ext(clean)
	stem := strings.TrimSuffix(clean, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, taken := used[candidate]; !taken {
			used[clean]++
			used[candidate] = 1
			return candidate
		}
	}
}

// openInFileManager opens the given directory in the host OS's native file
// manager. We deliberately avoid shelling out to anything that interprets the
// path (e.g. cmd.exe /c start "" %path%) and instead invoke the per-platform
// binary directly so callers don't have to worry about quoting.
//
// This is only used by the local dashboard and the manager binds to
// localhost — there's no remote-execution exposure even if someone tampers
// with the session id, because the path is always rooted at our artifacts
// directory and chi only allows a single URL segment in :id.
var openInFileManager = openFolderImpl

func openFolderImpl(dir string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", dir).Start()
	case "windows":
		// explorer.exe handles missing trailing slash fine. We use Start so
		// the explorer window persists after the manager process closes.
		return exec.Command("explorer.exe", dir).Start()
	default:
		// Linux / *BSD: xdg-open is the portable choice. If it isn't
		// installed the user gets a friendly error in the UI.
		return exec.Command("xdg-open", dir).Start()
	}
}

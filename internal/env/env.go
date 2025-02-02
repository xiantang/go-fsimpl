// Package env contains functions that retrieve data from the environment
package env

import (
	"io/fs"
	"os"
	"strings"
)

// Getenv - retrieves the value of the environment variable named by the key.
// If the variable is unset, but the same variable ending in `_FILE` is set, the
// referenced file will be read into the value.
// Otherwise the provided default (or an emptry string) is returned.
func Getenv(key string, def ...string) string {
	return getenvVFS(os.DirFS("/"), key, def...)
}

// getenvVFS - a convenience function intended for internal use only!
func getenvVFS(fsys fs.FS, key string, def ...string) string {
	val := getenvFile(fsys, key)
	if val == "" && len(def) > 0 {
		return def[0]
	}

	return val
}

func getenvFile(fsys fs.FS, key string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}

	p := os.Getenv(key + "_FILE")
	if p != "" {
		p = strings.TrimPrefix(p, "/")

		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return ""
		}

		return strings.TrimSpace(string(b))
	}

	return ""
}

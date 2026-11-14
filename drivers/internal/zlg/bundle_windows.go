//go:build windows

package zlg

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// bundle holds ZLG's zlgcan.dll and kerneldlls folder; see bundle/README.md.
//
//go:embed bundle/zlgcan.dll bundle/kerneldlls
var bundle embed.FS

// unpackBundle writes the bundle once per content version to the user's
// cache directory and returns its path. zlgcan.dll saves adapter settings
// beside itself, so the directory must stay writable. A complete directory is
// published by renaming, so concurrent processes never load a partial copy.
func unpackBundle() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	version, err := bundleVersion()
	if err != nil {
		return "", err
	}
	parent := filepath.Join(cache, "gocan", "zlg")
	directory := filepath.Join(parent, version)
	if _, err := os.Stat(directory); err == nil {
		return directory, nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, "unpack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	root, err := fs.Sub(bundle, "bundle")
	if err != nil {
		return "", err
	}
	if err := os.CopyFS(staging, root); err != nil {
		return "", err
	}
	// Security software briefly locks freshly written DLLs, which denies the
	// rename, and another process may publish the same version first.
	for attempt := 0; ; attempt++ {
		err := os.Rename(staging, directory)
		if err == nil {
			return directory, nil
		}
		if _, statErr := os.Stat(directory); statErr == nil {
			return directory, nil
		}
		if attempt == 20 {
			return "", err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func bundleVersion() (string, error) {
	hash := sha256.New()
	err := fs.WalkDir(bundle, "bundle", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := bundle.ReadFile(path)
		if err != nil {
			return err
		}
		hash.Write([]byte(path))
		hash.Write(data)
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil)[:8]), err
}

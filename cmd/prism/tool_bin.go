package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/webvictim/prism/internal/tshwrap"
)

// Only tools prism sets up get an override. For any other tool,
// `prism exec /path/to/it` already works.
var toolBinEnv = map[string]string{
	"claude":   "PRISM_CLAUDE_BIN",
	"codex":    "PRISM_CODEX_BIN",
	"opencode": "PRISM_OPENCODE_BIN",
	"pi":       "PRISM_PI_BIN",
}

// resolveToolBin swaps only the binary. Everything prism sets up for a
// tool is keyed on its name, so a binary called my-claude is still set
// up as claude. A bad override is an error, because falling back to
// PATH would quietly run something other than what was configured.
func resolveToolBin(tool string) (string, error) {
	if key, ok := toolBinEnv[tool]; ok {
		if bin := os.Getenv(key); bin != "" {
			if err := checkToolBin(bin); err != nil {
				return "", fmt.Errorf("%s=%s: %w", key, bin, err)
			}
			return bin, nil
		}
	}
	bin, err := tshwrap.LookPathStrict(tool)
	if err != nil {
		return "", fmt.Errorf("`%s` not found on PATH: %w", tool, err)
	}
	return bin, nil
}

// checkToolBin runs before the launch, so a bad value fails naming the
// variable rather than as a bare exec error partway through.
func checkToolBin(bin string) error {
	// A relative path would depend on the directory prism was started in.
	if !filepath.IsAbs(bin) {
		return errors.New("not an absolute path")
	}
	info, err := os.Stat(bin)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("no such file")
		}
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return pathErr.Err
		}
		return err
	}
	if info.IsDir() {
		return errors.New("is a directory")
	}
	// LookPath knows that Windows has no execute bit and goes by extension.
	if _, err := exec.LookPath(bin); err != nil {
		return errors.New("not executable")
	}
	return nil
}

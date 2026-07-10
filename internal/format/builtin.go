// =============================================================================
// File: internal/format/builtin.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-09
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Built-in Go formatting. The per-project format.json pipeline is
// opt-in and trust-gated because a cloned repo controls the argv; the
// built-in command is different in kind — it's hardcoded in this
// binary, points at well-known Go toolchain programs, and only ever
// touches the file the user just saved. Running it therefore needs no
// consent flow, the same way the editor runs `git` without asking.
//
// goimports is preferred over gofmt because it's a strict superset:
// it applies gofmt's formatting AND adds/removes import lines to
// match the code (the "auto-import" half of the feature). When
// goimports isn't installed we degrade to plain gofmt, and when
// neither is on PATH the save behaves exactly as before — silent
// degradation, the same contract as the LSP integration.

package format

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// lookPath is swappable in tests so builtin resolution doesn't depend
// on which Go tools the machine running the tests happens to have.
var lookPath = exec.LookPath

// builtinTools lists the Go formatters we know how to run, in
// preference order. Each takes `-w <file>` to rewrite in place, which
// is why one argv shape below serves both.
var builtinTools = []string{"goimports", "gofmt"}

// BuiltinCommandFor returns the built-in formatter argv for filePath,
// or nil when the file isn't Go or no Go formatter is installed. The
// argv carries the resolved binary path and the file's absolute path,
// ready for exec.Command.
//
// Precedence contract: a project format.json entry for "go" overrides
// this — callers must consult Config.CommandFor first and only fall
// back here when it returns nil.
func BuiltinCommandFor(filePath string) []string {
	if strings.TrimPrefix(filepath.Ext(filePath), ".") != "go" {
		return nil
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		abs = filePath
	}
	for _, tool := range builtinTools {
		if bin, lookErr := lookPath(tool); lookErr == nil {
			return []string{bin, "-w", abs}
		}
	}
	return nil
}

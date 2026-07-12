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
// goimports is preferred because it's a strict superset of gofmt: it
// applies gofmt's formatting AND adds/removes import lines to match
// the code (the "auto-import" half of the feature). When goimports
// isn't installed but gopls is, `gopls imports -w` provides the same
// import fixing (gopls is far more commonly installed than the
// standalone goimports these days), chained with a plain gofmt pass
// for the formatting half. With neither, plain gofmt still formats;
// with nothing on PATH the save behaves exactly as before — silent
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

// BuiltinCommandsFor returns the built-in formatter pipeline for
// filePath — a list of argvs to run in order, each rewriting the file
// in place — or nil when the file isn't Go or no Go tool is installed.
// Each argv carries the resolved binary path and the file's absolute
// path, ready for exec.Command. Preference order:
//
//	goimports -w            (format + fix imports, one tool)
//	gopls imports -w        (fix imports…)
//	  then gofmt -w         (…then format — two tools, same outcome)
//	gofmt -w                (format only; imports degrade silently)
//
// Precedence contract: a project format.json entry for "go" overrides
// this — callers must consult Config.CommandFor first and only fall
// back here when it returns nil.
func BuiltinCommandsFor(filePath string) [][]string {
	if strings.TrimPrefix(filepath.Ext(filePath), ".") != "go" {
		return nil
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		abs = filePath
	}
	if bin, lookErr := lookPath("goimports"); lookErr == nil {
		return [][]string{{bin, "-w", abs}}
	}
	var cmds [][]string
	if bin, lookErr := lookPath("gopls"); lookErr == nil {
		cmds = append(cmds, []string{bin, "imports", "-w", abs})
	}
	if bin, lookErr := lookPath("gofmt"); lookErr == nil {
		cmds = append(cmds, []string{bin, "-w", abs})
	}
	return cmds
}

# Session: Cut releases from a `release` branch instead of main push

Session ID: 67c68d30-f3fa-40f7-be40-96ea441f98dd
Date: 2026-07-15

## Goal

> "Don't trigger the release workflow on main push. Trigger on a new
> `release` branch. Let's cut that from main."

Re-wire `.github/workflows/release.yml` so releases are cut deliberately
by pushing a dedicated `release` branch, not by pushing `main`.

Committed on `main` (`57d2076`) and pushed. The `release` branch was cut
from `main` and pushed by the user in-session.

---

## Starting state

The release workflow was **already paused** during the spiceedit → r-ed
rebrand — its `on:` block had been reduced to `workflow_dispatch:` only,
with a big comment explaining how to restore `push: branches: [main]`.
So "don't trigger on main" was half-done; the real ask was to point the
push trigger at a new `release` branch and make the downstream steps
coherent with that.

## Decisions (asked the user)

1. **Where does the auto version-bump commit land?** → **`release`**
   (self-contained loop; `main` untouched). User will merge
   `release` → `main` themselves to bring `version.go` current.
2. **Cut + push the `release` branch?** → **create and push.**
3. **Push `release` now = a real v0.1.1 publish?** → **hold** (user
   later pushed it themselves via the `!` shell).

## Changes — `release.yml` (commit `57d2076`)

- `on:` → `push: branches: [release]` + kept `workflow_dispatch`.
  Removed the PAUSED/rebrand restore-instructions comment.
- **Version-bump push-back**: `git push origin HEAD:main` →
  `HEAD:release`. The tag `v<x.y.z>` is therefore cut from `release`.
- **Pages redeploy**: `gh workflow run pages.yml --ref main` →
  `--ref release`. Rationale: the bump now lands on `release`, so
  `pages.yml`'s path-filtered auto-deploy (gated on `main` +
  `version.go`) never fires for it. Dispatching on `--ref release`
  rebuilds the site from the exact released commit so the version badge
  matches the shipped binary without waiting for a `release → main`
  merge.
- Header / `on:` / permissions / step comments all reworded from "main"
  to "release", plus a note that `main` is left untouched by a release
  run.
- `permissions` unchanged (`contents: write` for the bump/tag push,
  `actions: write` for the pages dispatch).

## Changes — `CLAUDE.md`

Rewrote the **Releases (don't break this)** section: releases are cut by
pushing `release` (cut from main), bump commits back to `release`, pages
redeploys on the `release` ref, `main` untouched (merge back to refresh
its `version.go`). Added an explicit warning that **pushing `release` is
itself the trigger** — expect a real release on the very first push.

## The model, in one line

Develop on `main` → when ready, cut/`push origin release` → workflow
bumps + tags + releases + brew formula + site redeploy, all on/from
`release` → merge `release` back into `main` to un-stale its version.

---

## Post-push finding: **GitHub Actions is disabled repo-wide**

After the user pushed `release`, **no run started** — and investigation
showed **zero workflow runs have _ever_ executed in this repo**:

- `GET /actions/runs?per_page=…` → `total_count: 0` (all workflows).
- `GET /actions/workflows` → 3 workflows registered, all `state=active`
  (`Deploy site`, `Release`, `Test`).
- `Test` is configured to run on every push, yet never fired on any
  recent `main` push either → not specific to this change.
- Not rate-limiting: response had no `message`, valid JSON.
- `git ls-remote origin refs/heads/release` → `57d2076` (the updated
  workflow is on the pushed tip, so the wiring is correct).

**Conclusion:** Actions is turned off at the repository level (common
right after a fork/rebrand). Workflow files are registered but nothing
executes.

### Handoff / what the owner must do

1. **Settings → Actions → General → Actions permissions** → allow
   actions → Save.
2. Enabling does **not** retroactively run the already-pushed event.
   Trigger a fresh one, e.g.:
   ```sh
   git commit --allow-empty -m "Cut v0.1.1" && git push origin release
   ```
   or use **Actions → Release → Run workflow** (`workflow_dispatch`
   works because `release.yml` is on the default branch `main`).

## Tooling notes for next session

- `gh` CLI is **not installed** on this machine / PATH. Used the public
  GitHub REST API via `curl` + `python3` to inspect runs/workflows.
- No secrets in play; nothing to redact.

## Verification

- Edits are workflow YAML + docs only — no Go source touched, so no
  `go test` run (nothing to exercise). Read back `release.yml` to
  confirm the final `on:`/permissions block is coherent.
- Confirmed remote `release` tip == the updated commit.

# diskwise

A fast, zero-dependency CLI to find and clean up disk waste on macOS.

It walks `/System/Volumes/Data` in parallel, counts actual allocated blocks (so sparse VM disks and hard links aren't overcounted), matches ~55 common developer & macOS space hogs (Xcode, Cargo `target/`, `node_modules`, `CACHEDIR.TAG` caches, Docker/Podman/Tart, venvs, app caches), and lets you clean them up interactively. For unfamiliar folders, it can ask an LLM using only folder metadata—never file contents.

## Install

```sh
go build -o diskwise .
ln -sf "$PWD/diskwise" ~/.local/bin/diskwise
```

## Usage

```sh
diskwise scan                  # scan the Data volume and save ~/.cache/diskwise/scan.json
diskwise                       # show the report from the last scan
diskwise select                # interactive TUI: check items with SPACE, type CONFIRM + countdown
diskwise report --rule cargo-target
diskwise explain --unknown     # ask an LLM what the unrecognised buckets are
diskwise clean                 # dry-run list of safe cleanups (--apply to run one by one)
diskwise rules                 # list all built-in rules
```

## Safety

- **Rule categories:** `🟢` regenerable (rebuilt automatically), `🟡` costly (re-downloadable or slow to rebuild), `🔴` user data (report-only by default), `⛔` system (never touched).
- **Interactive cleanup (`diskwise select`):** toggle items with `SPACE`, press `ENTER`, type `CONFIRM`, and wait through a 5-second `Ctrl-C` countdown before anything is deleted.
- **Guardrails:** prefers native cleanup commands (`brew cleanup`, `docker system prune`, `go clean -cache`, etc.), re-verifies every path before deletion (must be inside `$HOME`, not a symlink, not a protected folder), and logs actions to `~/.local/state/diskwise/actions.log`.
- **Optional LLM (`diskwise explain`):** uses `claude` CLI, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` (+ `OPENAI_BASE_URL` for Ollama), `GEMINI_API_KEY`, or `DISKWISE_LLM_CMD`. It never executes LLM-suggested commands.

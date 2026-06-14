# tailagent

tailagent is a local workspace for managing Codex, Claude, Kiro, and Copilot CLI agents.
It runs as one Go binary, binds only to `127.0.0.1`, embeds its web UI, and
stores state in a local SQLite database.

## Requirements

- Go 1.26.3 (managed by `mise` in the development environment)
- A supported agent CLI in `PATH`: `codex`, `claude`, `kiro-cli`, or `gh` for Copilot

## Run locally

```sh
make run
```

Open <http://127.0.0.1:8787>. Data is stored in
`~/.config/tailagent/tailagent.db` by default.
`make run` opens the UI in the default browser automatically.

Options:

```text
-data-dir string   directory containing the SQLite database
-open              open the local UI in the default browser
-port int          localhost port (default 8787)
```

## Build and test

```sh
make test
make build
./bin/tailagent
```

The release build uses `CGO_ENABLED=0`, so the resulting executable has no
system SQLite dependency.

## Homebrew release

`.goreleaser.yaml` builds macOS binaries for Apple Silicon and Intel and
publishes the Formula to `kumagaias/homebrew-tap`. Configure a GitHub token
with write access to both repositories as the `TAP_GITHUB_TOKEN` Actions
secret before releasing.

```sh
git tag v0.1.0
git push origin v0.1.0
```

Pushing the tag triggers `.github/workflows/release.yml`.

## Local security model

- The HTTP server listens on `127.0.0.1` only.
- Agent processes run with the imported project as their working directory.
- Codex runs through `codex app-server`, allowing command, file-change, and
  additional-permission requests to be approved or denied in tailagent.
- stdout, stderr, approvals, and traces are retained in local SQLite.
- Common API token assignment patterns are redacted from trace messages.

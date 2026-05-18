# bach

Sandboxed Claude Code in a Linux container. Per-project network, allowlisted
HTTP/CONNECT proxy, optional backend services (postgres, redis, …) declared in
`.bach.toml`. macOS host today; Docker by default with an Apple `container` (microVM)
backend selectable per-project.

## Status

Single-machine, single-user, dev-only. Threat model is "Claude does dumb things",
not "Claude is actively malicious."

## Requirements

- macOS on Apple Silicon
- Docker (default backend), OR `container` (Apple's CLI; opt-in per project)
- `gh` CLI logged in (used for: propagating GitHub auth into the sandbox, and `docker login` for `ghcr.io`)

```sh
brew install docker gh
# Optional, for the Apple backend:
brew install --cask container
container system start --enable-kernel-install
```

## Install

```sh
git clone … bach
cd bach
docker build -t bach:base .
ln -s "$PWD/bach" ~/.local/bin/bach
```

The `bach-proxy` container image is built lazily on first use. On the Apple
backend the proxy is a host-side Go binary, built lazily on first use (requires
`go` installed).

## Use

From any project directory:

```sh
bach                    # interactive shell in a fresh session container
bach c                  # start claude under bash (Ctrl-Z suspends, fg resumes)
bach c --resume         # claude with args
bach run pnpm install   # one-off command in a fresh session
bach exec               # attach a shell to an already-running session
bach exec ls /work      # one-off command in a running session
bach ps                 # list running sessions for this project
bach up                 # start proxy + services from .bach.toml
bach down               # stop this project's services
bach status             # project, backend, proxy, services
bach log                # tail the bach-proxy log
bach build              # rebuild bach:base from this repo's Dockerfile
```

Without a `.bach.toml`, bach still works — no services, just the session
container with proxy and default-deny allowlist (every request is held for
manual approval on the dashboard).

The proxy dashboard lives at `http://127.0.0.1:8081/` — live event stream,
pending approvals, temp-allow controls.

## Source mode

Controlled by `source_mode` in `.bach.toml`. Three values:

- `auto` (default) — clone if the project has a `.git` directory, otherwise
  fall back to a plain RW bind mount with a warning.
- `clone` — must clone; error out if there's no `.git`.
- `bind` — always RW bind-mount the project root at `/work`.

In **clone** mode, the host's `.git` is mounted read-only at `/host-git`; the
entrypoint inits a worktree under `/work` backed by git alternates pointing at
the host's object store. Result: near-instant, near-zero extra disk, new
commits land in `/work/.git/objects` (ephemeral with the session container).
The session's `origin` is set to the host's `remote.origin.url`, rewritten to
HTTPS so `git push` works through the proxy with the propagated `GH_TOKEN`.

In **bind** mode, host edits are live inside the session and vice versa.

## GitHub auth

`bach` reads `gh auth token` on the host and exports it as `GH_TOKEN` inside
the session. The bach base image ships a system-wide git credential helper
(`!gh auth git-credential`) for `github.com` + `gist.github.com`, so `git push`
over HTTPS uses the same token without any extra config. `gh` itself uses the
same env var. For `ghcr.io` pulls, the same token is fed to `docker login`
(idempotent, once per process).

## Config (`.bach.toml`)

Walked up from the cwd. The directory containing it is the project root.

```toml
backend = "docker"                  # or "apple". Default docker.
source_mode = "auto"                # auto (default) | clone | bind. See *Source mode*.
image = "bach:base"                 # override the base image
mise_cache = true                   # share host mise tool cache (default true)

# Forwarded ports. bach auto-assigns a host port (first free from 4100
# upwards) per named entry and the proxy dashboard renders a clickable
# `http://localhost:<port>/` link. Allocations are sticky per (project,
# name) across restarts when free; a second concurrent session of the
# same project just gets the next free port. `host` pins a specific
# port; `scheme` (http|https, default http) controls the dashboard link.
[[ports]]
name = "web"
container = 4000
# host = 4100          # optional pin
# scheme = "https"     # optional, default http
apt_packages = ["build-essential"]  # → builds bach-proj:<hash> on top of bach:base

# Arbitrary install steps run during the per-project image build (alongside
# apt_packages). Build network is the host's, not the bach proxy.
[[install]]
name = "neovim-0.12.1"
script = """
curl -fsSL https://example.com/foo.tgz | tar -xz -C /opt
"""

# Hostname allowlist enforced by bach-proxy. Exact (`api.github.com`) or
# wildcard suffix (`*.githubusercontent.com` matches apex + any subdomain).
# Unset/empty = default-deny: every request is held for manual approval on the
# dashboard. Re-registered with the proxy on every `bach` / `bach up`.
allowlist = [
  "api.anthropic.com",
  "platform.claude.com",
  "api.github.com",
  "github.com",
  "*.githubusercontent.com",
  "uploads.github.com",
]

# Denylist: same pattern syntax as allowlist. Matching hosts are rejected
# immediately (no 5-second pending hold). User can still allow them ad-hoc
# from the dashboard — manual approvals always override the denylist.
denylist = [
  "*.tracker.example",
]

env = { RAILS_ENV = "development" }   # extra env vars in the session
volumes = ["build-cache:/home/agent/.cache/build"]
                                       # docker/container named volumes,
                                       # shared across every session that
                                       # references the same name. Auto-
                                       # chowned to agent on first creation.
                                       # Use this for writable state under
                                       # /home/agent — sidesteps VirtioFS
                                       # uid-preservation that bites bind
                                       # mounts there.

[aliases]                              # bash aliases inside the session
ll = "ls -la"

[[mounts]]                             # extra bind mounts
source = "../shared"                   # absolute, ~/..., or relative to project root
target = "/work/shared"
readonly = true                        # optional
# chown = true                         # entrypoint chowns target to agent.
                                       # Needed for writable mounts inside
                                       # /home/agent (VirtioFS preserves the
                                       # host uid otherwise). Do NOT enable
                                       # for host trees you care about —
                                       # chown propagates back to the host.

[[stage]]                              # copy a host file into the container
from = "~/.config/mise/age.txt"
to   = "/home/agent/.config/mise/age.txt"
mode = "0600"

[[cache]]                              # per-project build cache under /work
path = "deps"                          # relative to /work
mode = "overlay"                       # overlay | rw | ro
# overlay: host base bind-mounted at /bach-cache/<slug>; entrypoint rsyncs
#          base -> /work/<path> at start, /work/<path> -> base on clean exit.
#          Concurrent sessions don't corrupt each other mid-build.
# rw:      plain bind-mount of host cache dir at /work/<path>.
# ro:      same, read-only.

[services.postgres]                    # container on the project network,
image = "postgres:16-alpine"           # reached by short name (postgres)
env = { POSTGRES_USER = "postgres", PGDATA = "/var/lib/postgresql/data/pgdata" }
volumes = ["postgres-data:/var/lib/postgresql/data"]
volume_init_owner = "70:70"            # chown the named volume on first init
                                       # (only needed when the image runs as
                                       # non-root and ships a root-owned dir)

[services.redis]
image = "redis:7-alpine"
volumes = ["redis-data:/data"]
# command = ["redis-server", "--save", ""]
# platform = "linux/amd64"             # force Rosetta for amd64-only images
```

See `examples/example.bach.toml` for a multi-service example.

## User-level config (`~/.config/bach/config.toml`)

Same schema as `.bach.toml`. Layered **under** the project file (project wins
on conflicts). Use for things you want in every project: your editor, your
dotfiles, shared caches.

Merge rules:
- Arrays concat: `apt_packages`, `ports`, `allowlist`, `denylist`, `[[stage]]`,
  `[[mounts]]`, `[[cache]]`.
- Tables merge by key, later wins per key: `env`, `aliases`. `[services.<name>]`
  is replace-by-name (project-level service spec fully replaces user-level one
  with the same name; no deep merge of `env`/`volumes`/...).
- Scalars last-wins: `backend`, `image`, `mise_cache`.

Example — neovim in every session:

```toml
# ~/.config/bach/config.toml
apt_packages = ["neovim"]

[[mounts]]
source   = "~/.config/nvim"               # RO config (your nvim setup)
target   = "/home/agent/.config/nvim"
readonly = true

# Linux-only share dir, persisted across all bach sessions in a Docker
# named volume (lives in the Docker VM, not on the macOS host). Don't bind
# the host's ~/.local/share/nvim here — those are macOS-built plugins /
# treesitter parsers and will crash in the Linux container. The volume is
# auto-chowned to agent on first creation; no further setup needed.
volumes = ["nvim-share:/home/agent/.local/share/nvim"]

[aliases]
v = "nvim"
```

`~/.local/state/nvim` (swap, shada) is intentionally not mounted — it lives
inside the session container and is discarded on exit, which is fine.

Caveat: the share-dir mount is shared across all running sessions. For nvim
plugin files that's normally OK; for plugin-manager lock files (lazy-lock.json,
etc.) avoid running updates from two sessions concurrently.

## How it works

- **Network**: each project gets its own `--internal` network. No NAT to the
  internet. The only egress is via `bach-proxy`.
- **Proxy** (`bach-proxy`, Go, in `proxy/`): HTTP + CONNECT, per-project
  hostname allowlist enforced by source CIDR, DNS-rebinding protection (refuses
  resolutions that hit private ranges), 5s pending-approval window, 10min
  temp-allow on approval. Dashboard on `127.0.0.1:8081` (host-side only;
  containers can't reach it).
  - Docker: single dual-homed `bach-proxy` container on a shared `bach-internet`
    bridge plus every project network; reachable as `bach-proxy:8080` by DNS.
  - Apple: host-side binary; reachable at the per-project network gateway IP.
- **Source**: clone mode (default) or bind mount, set via `source_mode`. See *Source mode*.
- **Services** (`[services.<name>]`): each becomes a container named
  `bach-<project>-<svc>` on the project network. Reached from the session
  container by short name (`postgres`, `redis`, …). Docker uses bridge DNS;
  Apple injects `/etc/hosts` entries.
- **Credentials**: host's Claude Code OAuth token is extracted from the macOS
  keychain into a 0700 staging dir, mounted read-only at `/bach-runtime`; the
  entrypoint copies it into the ephemeral `~/.claude` of the session
  container. The host `~/.claude` is never bind-mounted.
- **Caches**: mise tool cache at `~/.local/share/bach/cache/mise/` is shared
  across all projects. Per-project conversation history at
  `~/.local/share/bach/projects/<proj>/`. Per-project build caches (`[[cache]]`)
  under `~/.local/share/bach/cache/projects/<proj>/`.
- **Per-project image**: when `.bach.toml` has `apt_packages`, bach generates a
  tiny `FROM bach:base` Dockerfile and tags `bach-proj:<hash>`. Hash includes
  the base image's digest, so a rebuilt base auto-invalidates project images.

## Host paths

```
~/.local/share/bach/cache/mise/                # shared mise tool cache
~/.local/share/bach/cache/projects/<proj>/     # per-project [[cache]] data
~/.local/share/bach/projects/<proj>/           # per-project claude history
~/.local/state/bach/proxy.{log,pid,err}        # host-side proxy state (Apple)
~/.local/state/bach/proxy-bin/bach-proxy       # host-built proxy binary (Apple)
~/.local/state/bach/runtime/                   # 0700 staged secrets + [[stage]]
~/.local/state/bach/build/<hash>/Dockerfile    # generated per-project image dirs
```

## Repo layout

```
bach              # Python 3 CLI (single file, stdlib only). Symlink to ~/.local/bin/bach.
Dockerfile        # bach:base. Debian trixie-slim + claude + mise + entrypoint.
entrypoint.sh     # Runs as root: seeds ~/.claude, applies BACH_HOSTS,
                  # [[stage]], [[cache]] warm/promote, then runuser to agent.
bach-rcfile       # Sourced by `bash --rcfile -i` when `bach c` runs claude
                  # under bash for job control.
proxy/            # bach-proxy: Go HTTP/CONNECT proxy with allowlist + SSE dashboard.
examples/         # Sample .bach.toml configs.
```

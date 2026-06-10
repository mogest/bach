# bach 🏖️

Run Claude Code in a sandbox, without giving up anything that makes it useful.

`bach` spawns Claude Code (or just a shell) inside a locked-down Linux
container, per project. Claude gets your project source, your language
toolchains, your backend services (postgres, redis, whatever you declare) —
but it can't touch the rest of your Mac, and every outbound network request
goes through an allowlisting proxy you control from a live dashboard.

The nice part: it feels like running `claude` normally. Your Claude
subscription, your GitHub auth, and your conversation history all carry over
automatically. You `cd` into a project, type `bach c`, and you're working.

## What you get

- **Real isolation** — each project runs on its own internal Docker network
  with zero direct internet access. The only way out is through `bach-proxy`,
  which enforces a per-project hostname allowlist.
- **A live dashboard** at `http://127.0.0.1:8081/` — watch requests stream by,
  approve unknown hosts with one click, see forwarded ports.
- **Safe-by-default source handling** — by default your project is *cloned*
  into the container (read-only `.git` + git worktree magic, so it's instant
  and costs ~no disk). Claude can't trash your working tree; changes only
  escape via `git push`.
- **Backend services from one TOML file** — declare postgres/redis/etc in
  `.bach.toml` and they're up, networked, and resolvable by short name.
- **No rebuild treadmill** — the Claude binary is cached on the host and
  mounted in, so Claude updates never require an image rebuild.

## Status

Single-machine, single-user, dev-only. The threat model is "Claude does dumb
things", not "Claude is actively malicious."

## Installation

You'll need about ten minutes, most of it waiting for the image build.

### 1. Prerequisites

- A Mac with **Apple Silicon**.
- **Docker** with a running daemon — [OrbStack](https://orbstack.dev/) or
  [Docker Desktop](https://www.docker.com/products/docker-desktop/) both work:

  ```sh
  brew install orbstack          # or: brew install --cask docker
  ```

  (Plain `brew install docker` only installs the CLI — you need a daemon too.)

- **Python 3.11+** on your PATH. Note the python3 bundled with macOS is too
  old (3.9), so if `python3 --version` says < 3.11:

  ```sh
  brew install python
  ```

- The **GitHub CLI**, logged in. bach uses it to pass your GitHub auth into
  the sandbox (so `git push` and `gh` just work) and to log in to `ghcr.io`:

  ```sh
  brew install gh
  gh auth login
  ```

- **Claude Code** installed on your Mac, with an active subscription. If you
  don't have it yet:

  ```sh
  curl -fsSL https://claude.ai/install.sh | bash
  ```

### 2. Get bach

Clone the repo and symlink the `bach` script somewhere on your PATH:

```sh
git clone https://github.com/mogest/bach.git
cd bach
mkdir -p ~/.local/bin
ln -s "$PWD/bach" ~/.local/bin/bach
```

If `~/.local/bin` isn't already on your PATH, add it to your shell profile:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

### 3. Build the base image

From anywhere (bach finds its own repo through the symlink):

```sh
bach build
```

This builds the `bach:base` Debian image. Takes a few minutes the first time.
The `bach-proxy` image builds itself automatically the first time you start a
session, so there's nothing else to build.

### 4. Connect your Claude subscription

One-time setup — mint a long-lived OAuth token and store it in your macOS
keychain:

```sh
bach setup-claude-token
```

This runs `claude setup-token`, which opens a browser window to authorise.
When it prints your `sk-ant-oat01-...` token, copy it and paste it at the
prompt. bach stores it in the keychain (service: `bach: Claude Code OAuth
Token`) and passes it into every session. It's valid for about a year and is
independent of your normal `claude` login, so host and sandbox sessions never
fight over credentials.

### 5. Take it for a spin

```sh
cd ~/code/some-project
bach c
```

First run downloads the Claude binary into the host cache and starts the
proxy — give it a few seconds. Then Claude opens, already trusted in `/work`,
ready to go.

Open the dashboard at **http://127.0.0.1:8081/** in a browser. When Claude
(or anything in the sandbox) tries to reach a host that isn't on the
project's allowlist, the request is held there for ~5 seconds waiting for
your click. Approve it and it's allowed for 10 minutes; ignore it and it gets
a 403. To stop approving the same hosts over and over, add them to
`allowlist` in the project's `.bach.toml` (see below).

That's it — you're installed. 🎉

## Everyday use

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
bach build              # rebuild bach:base (only needed when this repo changes)
```

A project doesn't need a `.bach.toml` — without one you still get the
sandboxed session and the proxy, just with no services and a default-deny
allowlist (every request held for manual approval on the dashboard). Add a
`.bach.toml` when you want allowlisted hosts, services, ports, or extra
packages.

Multiple sessions are fine: run several projects at once, or several sessions
of the same project side by side.

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
Uncommitted host changes aren't visible in the session — commit (or stash)
first, or use bind mode.

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
See `examples/example.bach.toml` for a working multi-service example.

```toml
source_mode = "auto"                # auto (default) | clone | bind. See *Source mode*.
image = "bach:base"                 # override the base image
mise_cache = true                   # share host mise tool cache (default true)
claude_model = "opus"               # pin claude's default model (unset = claude's own default)

# Forwarded ports. bach auto-assigns a host port (first free from 4100
# upwards) per named entry and the proxy dashboard renders a clickable
# `http://localhost:<port>/` link. Allocations are sticky per (project,
# name) across restarts when free; a second concurrent session of the
# same project just gets the next free port. `host` pins a specific
# port; `scheme` (http|https, default http) controls the dashboard link;
# `hostname` (default localhost) sets the host in the dashboard link —
# use a vanity name that resolves to localhost (e.g. via /etc/hosts).
[[ports]]
name = "web"
container = 4000
# host = 4100              # optional pin
# scheme = "https"         # optional, default http
# hostname = "app.local"   # optional, default localhost (dashboard link host)
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
- Scalars last-wins: `image`, `mise_cache`, `claude_model`.

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

## Troubleshooting

**"no Claude Code OAuth token in keychain"** — you skipped step 4. Run
`bach setup-claude-token`.

**Requests failing with 403 / curl stalling then failing** — the host isn't
on the project's allowlist. Open `http://127.0.0.1:8081/` and approve it
(lasts 10 minutes), or add the hostname to `allowlist` in `.bach.toml`
permanently. The agent inside the session knows to ask you to do this.

**`bach: command not found`** — `~/.local/bin` isn't on your PATH; see step 2.

**`ModuleNotFoundError: tomllib`** — your `python3` is older than 3.11;
`brew install python`.

**Image `bach:base` not found** — run `bach build` (step 3).

**Claude or a service can't be reached / weird network state** — `bach status`
to see what's running, `bach log` to tail the proxy, `bach down && bach up`
to bounce services.

## How it works

- **Network**: each project gets its own `--internal` network. No NAT to the
  internet. The only egress is via `bach-proxy`.
- **Proxy** (`bach-proxy`, Go, in `proxy/`): HTTP + CONNECT, per-project
  hostname allowlist enforced by source CIDR, DNS-rebinding protection (refuses
  resolutions that hit private ranges), 5s pending-approval window, 10min
  temp-allow on approval. Dashboard on `127.0.0.1:8081` (host-side only;
  containers can't reach it).
  A single dual-homed `bach-proxy` container sits on a shared `bach-internet`
  bridge plus every project network; reachable as `bach-proxy:8080` by DNS.
- **Source**: clone mode (default) or bind mount, set via `source_mode`. See *Source mode*.
- **Services** (`[services.<name>]`): each becomes a container named
  `bach-<project>-<svc>` on the project network. Reached from the session
  container by short name (`postgres`, `redis`, …) via Docker bridge DNS.
- **Credentials**: the long-lived Claude OAuth token is read from the macOS
  keychain on every session start and passed into the container as the
  `CLAUDE_CODE_OAUTH_TOKEN` env var. The host `~/.claude` is never
  bind-mounted; the session's Claude config is ephemeral.
- **Caches**: mise tool cache at `~/.local/share/bach/cache/mise/` is shared
  across all projects. Per-project conversation history at
  `~/.local/share/bach/projects/<proj>/`. Per-project build caches (`[[cache]]`)
  under `~/.local/share/bach/cache/projects/<proj>/`.
- **Claude binary**: not baked into the image. On session start bach resolves
  the current version (on the host — no proxy involved), downloads the
  linux binary once into `~/.local/share/bach/cache/claude/`, and mounts the
  cache read-only at `/bach-claude`; the entrypoint symlinks it to
  `~/.local/bin/claude`. Claude updates therefore need no image rebuild — not
  of `bach:base`, not of any per-project image. Offline, bach falls back to
  the newest cached version.
- **Per-project image**: when `.bach.toml` has `apt_packages`, bach generates a
  tiny `FROM bach:base` Dockerfile and tags `bach-proj:<hash>`. Hash includes
  the base image's digest, so a rebuilt base auto-invalidates project images.

## Host paths

```
~/.local/share/bach/cache/mise/                # shared mise tool cache
~/.local/share/bach/cache/claude/              # cached claude binaries (auto-updated)
~/.local/share/bach/cache/projects/<proj>/     # per-project [[cache]] data
~/.local/share/bach/projects/<proj>/           # per-project claude history
~/.local/state/bach/runtime/                   # 0700 staged secrets + [[stage]]
~/.local/state/bach/build/<hash>/Dockerfile    # generated per-project image dirs
```

## Repo layout

```
bach              # Python 3 CLI (single file, stdlib only). Symlink to ~/.local/bin/bach.
Dockerfile        # bach:base. Debian trixie-slim + mise + entrypoint (claude
                  # is mounted in from the host-side binary cache, not baked in).
entrypoint.sh     # Runs as root: seeds ~/.claude, applies [[stage]],
                  # [[cache]] warm/promote, then runuser to agent.
bach-rcfile       # Sourced by `bash --rcfile -i` when `bach c` runs claude
                  # under bash for job control.
proxy/            # bach-proxy: Go HTTP/CONNECT proxy with allowlist + SSE dashboard.
examples/         # Sample .bach.toml configs.
```

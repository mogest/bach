# bach

Sandboxed Claude Code in a Linux container. Pluggable backend (Docker, default; Apple `container` CLI as a microVM option) + bach-proxy (Go, allowlisted HTTP/CONNECT proxy with SSE dashboard) + a Debian image. Per-project network, per-project optional image, bind-mounted source.

## What bach does

`bach` from a project dir spawns a session container (default: interactive shell; `bach c` for claude). The container:

- Has the project source available at `/work`. Controlled by `source_mode` in `.bach.toml` — `auto` (default) clones when `.git` is present and falls back to bind otherwise; `clone` is strict (error if no `.git`); `bind` always RW-binds the project root. **Clone mode**: host's `.git` mounted read-only at `/host-git`, entrypoint inits a worktree under `/work` backed by git alternates → near-instant, near-zero extra disk, new commits ephemeral with the session unless pushed.
- Has zero direct internet egress — the project network is `--internal`. Outbound HTTP/HTTPS goes through bach-proxy. One proxy serves the whole system; on Docker that's a dual-homed proxy container reachable as `bach-proxy:8080`, on Apple it's a host-side `bach-proxy` binary reachable at each project network's gateway. **Per-project hostname allowlist** (from `.bach.toml` `allowlist = [...]`), DNS-rebinding protection (refuses RFC1918 resolutions), 5s pending-approval window, 10min temp-allow on approval, dashboard at `http://127.0.0.1:8081/`. Default-deny: no global allowlist — projects without a configured allowlist hold every request for manual approval.
- Has backend services (postgres, redis, etc.) from `.bach.toml` running on the same network, reachable from the session container by short name (Docker resolves natively via bridge DNS; Apple injects `/etc/hosts` entries from `BACH_HOSTS`).
- Inherits the host's Claude Code auth without bind-mounting `~/.claude`. The wrapper extracts the OAuth token from macOS keychain into a staging dir, mounts that read-only, entrypoint copies the credential file into the container's own `~/.claude` (which is ephemeral with `--rm`).
- Inherits the host's GitHub auth: `gh auth token` exported as `GH_TOKEN`. The base image bakes in a system-wide git credential helper (`!gh auth git-credential`) for `github.com` + `gist.github.com`, so `git push` over HTTPS Just Works inside the sandbox using the same token.
- Persists conversation history under `~/.local/share/bach/projects/<project>/` (bind mount, allows concurrent sessions on different projects without volume contention).
- Persists mise tools at `~/.local/share/bach/cache/mise/` (bind mount, shared across projects).
- Supports per-project caches under `~/.local/share/bach/cache/projects/<proj>/<slug>/` via `[[cache]]`, mode `rw` / `ro` / `overlay`. Overlay mode rsyncs base → `/work/<path>` at start and promotes back on clean exit (last-writer-wins), keeping per-session writes isolated mid-build.

## Repo layout

```
bach              # Python 3 CLI (single file, stdlib only). Symlinked to ~/.local/bin/bach.
Dockerfile        # bach:base. Debian trixie-slim + claude + mise + gh credential helper + entrypoint.
entrypoint.sh     # Runs as root: seeds ~/.claude (+ gh config + onboarding), applies BACH_HOSTS,
                  # BACH_ALIASES, [[stage]] manifest, clone-mode worktree init,
                  # [[cache]] overlay warm/promote, exports HOME/USER/LOGNAME/SHELL,
                  # then runuser --preserve-environment to agent.
bach-rcfile       # Sourced by `bash --rcfile -i` when `bach c` runs claude under bash for
                  # job control (Ctrl-Z suspends claude → bash; fg resumes).
pbcopy            # /usr/local/bin/pbcopy inside the image. macOS pbcopy(1) workalike
                  # that writes stdin to the host clipboard via the OSC 52 terminal
                  # escape — requires the user's terminal to support OSC 52 writes.
proxy/            # bach-proxy: Go HTTP/CONNECT proxy with allowlist + dashboard.
                  # main.go, main_test.go, go.mod, Dockerfile.
examples/         # Sample .bach.toml configs.
README.md         # User-facing docs.
```

## Host paths (XDG)

- `~/.local/share/bach/cache/mise/` — mise tool cache (bind mount into all sessions)
- `~/.local/share/bach/cache/projects/<proj>/<slug>/` — per-project `[[cache]]` host base
- `~/.local/share/bach/projects/<proj>/` — claude conversation history per project (bind mount)
- `~/.local/state/bach/proxy.log` — bach-proxy log (Apple backend; docker backend uses `docker logs bach-proxy`)
- `~/.local/state/bach/proxy.{pid,err}` — host-side proxy process state (Apple)
- `~/.local/state/bach/proxy-bin/bach-proxy` — host-built proxy binary (Apple)
- `~/.local/state/bach/runtime/` — staged secrets (mode 0700); credentials + `[[stage]]` files + cache-manifest
- `~/.local/state/bach/build/<hash>/Dockerfile` — generated per-project image build dirs
- `~/.local/state/bach/ports.json` — sticky port allocations per (project, name)
- `~/.local/state/bach/ports.lock` — flock'd while allocating/releasing ports

## Config (`.bach.toml` + `~/.config/bach/config.toml`)

Walked up from `cwd`. Project root = the dir containing `.bach.toml` (or `cwd` if none).

A user-level file at `$XDG_CONFIG_HOME/bach/config.toml` (default `~/.config/bach/config.toml`) is loaded first and the project's `.bach.toml` is layered on top. Same schema. Merge in `merge_configs()`:
- Lists concat: `apt_packages`, `ports`, `allowlist`, `denylist`, `stage`, `mounts`, `cache`, `volumes`, `install`.
- Tables merge by key, later wins per key: `env`, `aliases`. `services` is replace-by-name (collision = project's spec wins entirely, no deep merge).
- Scalars last-wins (`backend`, `image`, `mise_cache`).

`[[mounts]].source` is `expanduser`'d, so user-level mounts can use `~/...` (e.g. `~/.config/nvim`). Relative sources still resolve against the project root. Optional `chown = true` per mount adds the target to `BACH_CHOWN` env (newline-separated); the entrypoint chowns each non-recursively to agent. Needed for writable bind mounts under `/home/agent` because VirtioFS preserves the host uid on bind mounts (a host dir owned by 501:20 appears as 501:20 inside the container, so agent/1000 can't write). Non-recursive is enough: future files agent creates inherit agent ownership. Ignored when `readonly = true`. Do not enable for host trees outside the bach cache area — chown propagates back to the host via VirtioFS.

```toml
backend = "docker"                                 # or "apple". Default docker.
image = "bach:base"                                # override the session image
mise_cache = true

# Forwarded ports. bach auto-assigns a host port (starting at 4100) per
# named entry; the proxy dashboard shows the assigned URL. Allocations are
# sticky per (project, name) across restarts when free, otherwise the next
# free port wins. Optional `host` pins a specific host port; optional
# `scheme` (http|https, default http) controls the dashboard link.
# State lives at ~/.local/state/bach/ports.json. Cleanup: bach releases
# its own session's ports on exit; every bach invocation also reaps
# entries whose session container is no longer running.
[[ports]]
name = "web"
container = 4000
# host = 4100        # optional pin
# scheme = "https"   # optional, default "http"
apt_packages = ["build-essential", "libatomic1"]   # builds bach-proj:<hash> on top of bach:base

# Arbitrary install scripts during per-project image build. Each script gets
# written to install-<i>.sh in the build context and run as `set -eu`. Build
# network is the host's, not the bach proxy. Hash includes script contents
# so edits rebuild.
[[install]]
name = "neovim-0.12.1"
script = """
curl -fsSL https://example/foo.tgz | tar -xzC /opt
"""
env = { RAILS_ENV = "development" }                # extra env in the session
volumes = ["nvim-share:/home/agent/.local/share/nvim"]
                                                   # named volumes (live in the docker/container VM,
                                                   # NOT VirtioFS), namespaced bach-shared-<name>,
                                                   # shared across every session referencing the name,
                                                   # auto-chowned to agent on first creation.
                                                   # Prefer over writable [[mounts]] for state under
                                                   # /home/agent (bind mounts there hit VirtioFS uid
                                                   # preservation; in-container chown often no-ops).

# Hostname allowlist enforced by bach-proxy. Default-deny: an unset/empty
# list means every request from this project is held for manual approval on
# the dashboard (http://127.0.0.1:8081/). Patterns are exact (`api.github.com`)
# or wildcard suffix (`*.githubusercontent.com` matches both the bare apex and
# any subdomain). Re-registered on every `bach` / `bach up`.
allowlist = [
  "api.anthropic.com",
  "platform.claude.com",
  "api.github.com",
  "github.com",
  "*.githubusercontent.com",
  "uploads.github.com",
]

# Denylist: same pattern syntax as allowlist. Matching hosts skip the pending
# hold and are rejected immediately. A dashboard approval still overrides
# (temp-allow is checked before denylist), so the user can ad-hoc allow a
# denied host.
denylist = ["*.tracker.example"]

[aliases]                       # bash aliases inside the session
ll = "ls -la"

[[mounts]]                      # extra bind mounts; relative source resolved against project root
source = "../shared"
target = "/work/shared"
readonly = true

[[stage]]                       # host file → container path (mode 0700 staging, copied by entrypoint)
from = "~/.config/mise/age.txt"
to = "/home/agent/.config/mise/age.txt"
mode = "0600"

[[cache]]                       # per-project build cache under /work
path = "deps"                   # relative to /work
mode = "overlay"                # overlay (default) | rw | ro
# overlay = host base bind-mounted at /bach-cache/<slug>; entrypoint rsyncs
#           base → /work/<path> at start, /work/<path> → base on clean exit.
# rw/ro   = plain bind-mount of host cache dir at /work/<path>.

[services.postgres]
image = "postgres:16-alpine"
env = { POSTGRES_USER = "postgres", PGDATA = "/var/lib/postgresql/data/pgdata" }
volumes = ["postgres-data:/var/lib/postgresql/data"]
volume_init_owner = "70:70"     # one-shot alpine chown of the named volume on first init
                                # (needed when image runs as non-root with a root-owned volume dir)

[services.rustfs]
image = "rustfs/rustfs:latest"
volumes = ["rustfs-data:/data"]
volume_init_owner = "10001:10001"
```

## Test bed

A real-world project with a working `.bach.toml` is used for iteration. Typical service mix: postgres, redis, dynamodb, rustfs (volume-init pattern), plus a private ghcr.io image (works under Docker backend via `gh auth token` → `docker login`).

## Backends

Backend selection: `.bach.toml` key `backend = "docker"` (default) or `"apple"`. Implementation in `bach` as `DockerBackend` / `AppleBackend` subclasses of `Backend`. Adding a third backend means another subclass; call sites use `backend.<method>`.

**Why Docker is the default**: Apple container has a known keychain bug (apple/container#1253 / PR #1257) — the `container-core-images` helper subprocess can't read credentials the CLI just wrote because they live in different keychain access groups. Pull of any private image fails with `errSecInteractionNotAllowed (-25308)`. Until that fix ships, Apple backend is unusable for any project that needs private images. The bach `AppleBackend` keeps the registry-login plumbing wired up so it'll Just Work once #1257 lands.

**Docker backend specifics**:
- Proxy is a single shared container (`bach-proxy`, locally-built `bach-proxy:latest`) on a shared `bach-internet` bridge plus every project's `--internal` network. Containers reach it by DNS (`bach-proxy`). The internal network has no NAT — direct egress is genuinely blocked, only the proxy has dual-homed egress. Same isolation property as Apple's gateway model, different mechanism.
- Proxy image is built lazily on first `bach up` from `proxy/Dockerfile`. The proxy container is labeled with the source image digest; bach recreates the container when the image is rebuilt so code changes propagate.
- Dashboard port (8081) is published to `127.0.0.1` on the host only. Inside the proxy, the dashboard listener checks the local-arrival IP against `BACH_DASH_ALLOW_CIDR` (= bach-internet's subnet, resolved at start); a session container hitting `bach-proxy:8081` lands on the project-network interface and is rejected. Belt + braces.
- **TCP forwarder for `[[ports]]`.** `--internal` networks silently drop `-p` publishing, so the proxy container itself pre-publishes a contiguous host port range (`PORT_ALLOC_START..PORT_ALLOC_END`, default 4100–4199; bumped via `BACH_FORWARD_RANGE` env on the proxy and matching constants on the bach side). bach POSTs `{project, forwards:[{host_port, container_ip, container_port}]}` to `/forwards` after `docker run`; the proxy starts a listener per host port and TCP-forwards to the session container's IP on its `--internal` network. The forwarder uses the same local-arrival-CIDR check as the dashboard — session-to-session loop-back via `bach-proxy:<port>` is rejected on accept. The proxy container is re-created when the port range changes (label `bach.proxy.port_range`).

**Proxy ↔ bach control plane**:
- bach POSTs each project's source CIDR + allowlist + denylist (+ port allocations for the dashboard) to the proxy dashboard endpoint `POST /project` (gated by the same dashboard-CIDR check) after every `ensure_proxy` call. The proxy keeps an in-memory `CIDR → Project{name, patterns, denylist, ports}` map and uses `r.RemoteAddr` to look up which project a request belongs to. Authorization order per request: allowlist → temp-allow → denylist (immediate reject, no pending hold) → pending. The dashboard exposes a `GET /ports` JSON view of the per-project allocations.
- bach POSTs `{project, forwards}` to `POST /forwards` whenever the live forward set changes (session start, session exit, reap). The proxy replaces this project's contribution to the forwarder map; listeners not referenced by any project are closed. Out-of-range host ports are rejected with a log line.
- Re-registration is fire-and-forget on every `bach <anything-that-spawns>` and `bach up`. If the proxy container restarts (and loses memory), the next bach action re-registers. Edge case: an in-flight session container talking to a freshly-restarted proxy with no registration will see its requests held as pending until the next bach invocation; not worth persisting registrations to disk yet.
- Project name on the dashboard = the project root directory name (e.g. `myproj`), not the docker network name (`bach-myproj`).
- Approvals + temp-allow entries are scoped by `project|host`, so allowing `api.foo.example` from project A does not allow it from project B.
- Service-name DNS is native to Docker bridge networks; `BACH_HOSTS` injection is skipped.
- Volume ownership inherits from image FS on first mount, which is the wrong thing for images whose runtime uid differs from their on-disk uid (e.g. `amazon/dynamodb-local`: image dir is root-owned, process runs as dynamodblocal/1000 → SQLite fails to open). `volume_init_owner` runs a one-shot alpine `chown -R` on the named volume; same impl on both backends.
- Registry auth: idempotent `docker login` from `gh auth token` once per process. Don't trust `~/.docker/config.json`'s `auths.<host>: {}` — that entry doesn't carry the credential, which lives in the OS cred helper and may be stale.

**Apple backend quirks** (don't unwind without reason):

1. **Volumes start root-owned with `lost+found`.** Apple container doesn't copy image-dir ownership on first mount. `volume_init_owner` triggers a pre-chown via a one-shot alpine container, and postgres uses `PGDATA=<subdir>` to avoid `lost+found`.
2. **No automatic DNS for container names on a custom network.** Wrapper inspects each service's `ipv4Address` after start, writes `BACH_HOSTS` env, entrypoint appends to `/etc/hosts`.
3. **No `--add-host` flag on `container run`.** Hence the env-var/entrypoint dance above.
4. **Same named volume can't mount into two containers concurrently.** This is why mise cache and projects history are bind mounts, not named volumes. Service data volumes (postgres etc) stay as named volumes — single-tenant per project.
5. **`container container inspect` is wrong**; container inspect is the top-level `container inspect <name>`. The wrapper's `inspect()` special-cases `container` vs `network`/`volume`/`image`.
6. **`-it` without a real TTY fails with NSPOSIXErrorDomain Code=19.** Wrapper checks `sys.stdin.isatty()` and picks `-i` or `-it`. (Docker has the same fix; harmless on both.)
7. **The host gateway IP per network is `<subnet>.1`.** Wrapper reads it from `container network inspect <net>.status.ipv4Gateway`.
8. **Registry auth doesn't survive the helper-subprocess hop.** See above.

## Container env / privilege model

- `Dockerfile` ends with `USER root` and `ENTRYPOINT [tini, --, entrypoint.sh]`. Entrypoint runs as root, does privileged setup (`/etc/hosts`, `~/.claude` seeding, `[[stage]]` placement), then `exec runuser --preserve-environment -u agent -- "$@"`.
- `runuser --preserve-environment` keeps env vars **except** `HOME`, `USER`, `LOGNAME`, `SHELL`. The entrypoint exports those manually before the runuser call (this was a real bug — without it, claude looked for credentials at `/root/.claude/.credentials.json`).
- `agent` (uid 1000) never has sudo. Privilege-needing work happens in the entrypoint, never in the user command.

## Per-project image

When `.bach.toml` has `apt_packages`, the wrapper generates a tiny Dockerfile (`FROM bach:base` + apt install) and builds `bach-proj:<hash>`. Hash includes the base image's index digest so rebuilding `bach:base` auto-invalidates project images on next session.

## Network lockdown

- Project network created with `--internal` (both backends). No NAT to internet from inside the project network.
- Docker: session container reaches the dual-homed `bach-proxy` by DNS over the internal network. Direct egress attempts fail at DNS (curl `Could not resolve host`). Verified.
- Apple: session container reaches host bach-proxy via the network gateway IP. Resolv.conf points there; DNS only resolves via proxy-tunnelled CONNECTs. Verified (`HTTPS_PROXY= curl …` times out with rc=28).
- `HTTPS_PROXY` / `HTTP_PROXY` (+ lowercase) set per-backend. NO_PROXY includes the proxy hostname/IP and service short names.
- Claude, npm, git over HTTPS, curl — all respect `HTTPS_PROXY`. SSH/git+ssh outbound is blocked (no path) — not a goal yet.

## Subcommands

- `bach` (or `bach shell` / `bach s`) — default. Spawns an interactive `bash -l` in a fresh session container.
- `bach claude` / `bach c` — start claude. With no args, runs under `bash --rcfile /etc/bach-rcfile -i` so Ctrl-Z suspends claude to a shell (fg resumes). With args, runs `claude <args>` directly.
- `bach run <cmd>...` — one-off command in a fresh session container, no TTY wrapping.
- `bach exec [--to <name>] [cmd...]` (alias `e`) — exec into an already-running session as `agent` in `/work`. Default cmd: `bash -l`. With multiple sessions and a TTY, presents a picker; otherwise `--to <name>` or `$BACH_SESSION`.
- `bach ps` — list this project's running session containers.
- `bach up` / `bach down` — start/stop the proxy + backend services for the project.
- `bach status` — project, backend, proxy, and service state.
- `bach log` — `docker logs --tail 50 bach-proxy` (Docker) / `tail -50 ~/.local/state/bach/proxy.log` (Apple).
- `bach build` — rebuild `bach:base` from this repo's Dockerfile. Per-project images auto-invalidate on next session (their hash includes the base image digest).

Source mode is set via `.bach.toml` `source_mode = "auto" | "clone" | "bind"` (default `auto`). No CLI flag — was previously `--no-clone`, removed in favour of the TOML knob.

## What's deferred

In order of how much value vs how much effort:

1. **Persist proxy project registrations across proxy restarts.** Currently the proxy holds its `CIDR → Project` map in memory; if the container restarts, in-flight session requests see `project=?` and hit a default-deny allowlist until the next bach invocation. Snapshot/restore via a JSON file under `~/.local/state/bach/` would close that gap.
2. **Per-project local override (`.bach.local.toml`).** User-level config is in; per-project uncommitted override is the natural follow-on (for per-project tweaks teammates shouldn't see). Same merge code applies; order would be user < project < project-local. Add a `.gitignore` auto-append when first detected.
3. **`bach prune` / `bach rebuild`.** Old per-project images, old build dirs, old service containers — accumulate over time.
4. **More registries beyond ghcr.io.** `ensure_registry_auth` is ghcr.io-specific today. Extend to AWS ECR (`aws ecr get-login-password`), GAR, etc. when needed.
5. **`--dangerously-skip-permissions`.** The entrypoint already writes `permissions.defaultMode = "auto"` into the session's `~/.claude/settings.json`, which covers most of the value. Passing the CLI flag explicitly (or making it `.bach.toml` `skip_permissions = true`) is a small follow-up.
6. **`[[cache]]` with arbitrary target path.** Today `[[cache]].path` is hard-rooted at `/work/<path>`, so a user-level nvim plugin cache has to use `[[mounts]]` instead of `[[cache]]`. Adding an optional `target` (absolute container path, escapes /work) would let overlay-mode warm/promote work for HOME-rooted caches too.

Recently shipped (don't reintroduce as deferred): clone-RO source mode is the default, configured via `.bach.toml` `source_mode = "auto" | "clone" | "bind"` (the old `--no-clone` CLI flag was replaced); README rewrite; gh credential helper + `GH_TOKEN` propagation; user-level `~/.config/bach/config.toml` with layered merge.

## Design decisions made, don't relitigate without reason

- **No bind-mount of host `~/.claude`.** Container's claude config is ephemeral. Credentials are copied in from a staging dir; conversation history is its own bind mount under `~/.local/share/bach/projects/<proj>/`; the host's `CLAUDE.md` is staged in with a bach sandbox-awareness suffix appended (so the agent knows how to ask the user to approve a host via the proxy dashboard); skills are opt-in via `[[stage]]`. Settings, history, etc. are not exposed.
- **No sudo for `agent`.** Privileged ops happen in entrypoint as root, then drop. Reason: user explicitly stated "I never want to run anything as root" for service containers — kept the principle for the session container too.
- **XDG paths, not `~/.bach/`.** Reason: user preference (`~/.bach` style is dated).
- **Bind mounts for mise cache + projects history**, named volumes only for per-project service data. Reason: Apple container doesn't allow concurrent named-volume mounts, but the whole point of bach is parallel sessions.
- **Threat model is "claude does dumb things, not actively malicious."** Default source mode is clone-from-host-RO (`.git` mounted read-only, worktree backed by alternates; changes only persist via `git push`), which already covers most of the "stop claude from blasting the host tree" case. `source_mode = "bind"` falls back to a RW bind for users who want host-side edits live. Sandbox isolates blast radius from host, not session-to-session. If threat shifts to active-malicious, the stricter knobs left are: force `source_mode = "clone"` everywhere, tighten the proxy allowlist per-project, and revisit `agent` having any FS writes outside `/work`.

## Recent session (this one) achievements

End-to-end working from zero:
- Image built, claude works, mise works, sops via `[[stage]]` works, all configured services up.
- Trust prompt suppressed for `/work`. Onboarding seeded.
- Network properly locked down — verified bypass attempts fail.
- Bash job control around claude.
- Two concurrent sessions on different projects: works.
- Two concurrent sessions on the same project: works; second one skips port-publishing if host port is taken.

## Coding conventions in `bach` (the Python script)

- stdlib only, no third-party deps (so it runs from a symlink with no venv).
- Subprocess `container` / `docker` calls — never an SDK. The CLI is the contract.
- Backend-specific code lives behind the `Backend` abstraction; call sites are backend-agnostic (`backend.cli`, `backend.container_state(...)` etc).
- Print short single-line status lines prefixed `bach: ` to stderr/stdout for the user. Don't be chatty.
- TOML config is the source of truth; the wrapper is mostly translation to `container run` / `docker run` flags.

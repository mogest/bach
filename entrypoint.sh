#!/bin/bash
set -euo pipefail

# CA certs (future MITM support: bach mounts certs at /certs).
if [ -d /certs ] && compgen -G "/certs/*.crt" > /dev/null 2>&1; then
    cp /certs/*.crt /usr/local/share/ca-certificates/ 2>/dev/null || true
    update-ca-certificates >/dev/null 2>&1 || true
    first_cert=$(ls /certs/*.crt | head -1)
    export NODE_EXTRA_CA_CERTS="$first_cert"
fi

# Claude auth comes from CLAUDE_CODE_OAUTH_TOKEN in the env (a long-lived
# setup-token minted on the host via `claude setup-token`, stashed in the
# host keychain by `bach setup-claude-token`). We don't write a credentials
# file at all — the env var is sufficient and sidesteps the OAuth refresh
# race that happens when multiple short-lived-token clients refresh in
# parallel.
CLAUDE_DIR=/home/agent/.claude
mkdir -p "$CLAUDE_DIR/projects" /home/agent/.local/share/mise
# chown is best-effort: bind-mounted dirs may reject it but already appear as
# agent inside the container due to host->container uid mapping.
chown agent:agent "$CLAUDE_DIR" 2>/dev/null || true
chown agent:agent "$CLAUDE_DIR/projects" 2>/dev/null || true
chown agent:agent /home/agent/.local/share/mise 2>/dev/null || true

# Staged CLAUDE.md: host's ~/.claude/CLAUDE.md (if any) + bach sandbox suffix
# appended by the wrapper. Tells the agent it's sandboxed and how to ask for
# a host to be approved through bach-proxy.
if [ -f /bach-runtime/CLAUDE.md ]; then
    cp /bach-runtime/CLAUDE.md "$CLAUDE_DIR/CLAUDE.md"
    chown agent:agent "$CLAUDE_DIR/CLAUDE.md"
    chmod 644 "$CLAUDE_DIR/CLAUDE.md"
fi

cat > "$CLAUDE_DIR/settings.json" <<'EOF'
{
  "skipAutoPermissionPrompt": true,
  "awaySummaryEnabled": false,
  "model": "opus",
  "permissions": {
    "defaultMode": "auto"
  }
}
EOF
chown agent:agent "$CLAUDE_DIR/settings.json"

# Mirror host git identity so commits made inside the session have the same
# author as ones the user makes on the host. Wrapper reads `git config
# --global user.{name,email}` and forwards them via these env vars; system
# scope so the value applies to any user (only agent runs git anyway).
if [ -n "${BACH_GIT_NAME:-}" ]; then
    git config --system user.name "$BACH_GIT_NAME"
fi
if [ -n "${BACH_GIT_EMAIL:-}" ]; then
    git config --system user.email "$BACH_GIT_EMAIL"
fi

# Seed gh config so commands like `gh repo clone owner/repo` default to HTTPS
# (matching the credential helper). Auth itself comes from GH_TOKEN env set
# by the wrapper.
mkdir -p /home/agent/.config/gh
cat > /home/agent/.config/gh/config.yml <<'EOF'
git_protocol: https
prompt: disabled
EOF
chown -R agent:agent /home/agent/.config/gh
cat > /home/agent/.claude.json <<'EOF'
{
  "hasCompletedOnboarding": true,
  "projects": {
    "/work": {
      "hasTrustDialogAccepted": true
    }
  }
}
EOF
chown agent:agent /home/agent/.claude.json

# BACH_CHOWN: newline-separated container paths to chown to agent. Bach adds
# writable [[mounts]] targets that opt in with `chown = true`. Non-recursive
# is intentional: VirtioFS preserves the host uid for bind-mounted dirs, so
# agent can't write inside until the dir itself is agent-owned; files agent
# creates later inherit agent ownership and don't need re-chowning each
# session.
if [ -n "${BACH_CHOWN:-}" ]; then
    while IFS= read -r d; do
        [ -z "$d" ] && continue
        chown agent:agent "$d" 2>/dev/null || true
    done <<< "$BACH_CHOWN"
fi

# BACH_ALIASES: newline-separated `alias k='v'` lines from [aliases] in
# .bach.toml. Write to /etc/profile.d/ (sourced by `bash -l` via /etc/profile)
# and append a source line to ~/.bashrc (which bach-rcfile sources for the
# interactive `bash --rcfile` shell used by `bach c`).
if [ -n "${BACH_ALIASES:-}" ]; then
    mkdir -p /etc/profile.d
    printf '%s\n' "$BACH_ALIASES" > /etc/profile.d/bach-aliases.sh
    chmod 644 /etc/profile.d/bach-aliases.sh
    echo '. /etc/profile.d/bach-aliases.sh' >> /home/agent/.bashrc
fi

# [[stage]] files/dirs declared in .bach.toml. Manifest format per line:
#   <staged_name>\t<target_path>\t<mode>\t<kind>   where kind is "f" or "d".
# Files: copy + chmod mode. Dirs: recursive copy of contents into target,
# chown -R to agent; mode is currently ignored for dir entries.
if [ -s /bach-runtime/stage/manifest ]; then
    while IFS=$'\t' read -r src target mode kind; do
        [ -z "$src" ] && continue
        spath="/bach-runtime/stage/$src"
        case "${kind:-f}" in
            d)
                if [ -d "$spath" ]; then
                    mkdir -p "$target"
                    cp -RT "$spath" "$target"
                    chown -R agent:agent "$target"
                fi
                ;;
            *)
                if [ -f "$spath" ]; then
                    target_dir=$(dirname "$target")
                    mkdir -p "$target_dir"
                    chown agent:agent "$target_dir"
                    cp "$spath" "$target"
                    chmod "$mode" "$target"
                    chown agent:agent "$target"
                fi
                ;;
        esac
    done < /bach-runtime/stage/manifest
fi

export HOME=/home/agent
export USER=agent
export LOGNAME=agent
export SHELL=/bin/bash
export PATH="/home/agent/.local/bin:/home/agent/.local/share/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Clone mode: host's .git is mounted read-only at /host-git. We skip
# `git clone` (which silently disables --local/--shared across mount points
# and ends up copying packs anyway) and manually init a worktree backed by
# alternates pointing at the host's objects. Result: ~instant, ~0 extra disk,
# new commits land in /work/.git/objects. Caveat: `git gc --prune=now` on
# host mid-session can evict objects the alternates depend on.
if [ -n "${BACH_CLONE_SRC:-}" ]; then
    chown agent:agent /work
    runuser -u agent -- git init --quiet /work
    mkdir -p /work/.git/objects/info
    echo "$BACH_CLONE_SRC/objects" > /work/.git/objects/info/alternates
    rm -rf /work/.git/refs
    cp -r "$BACH_CLONE_SRC/refs" /work/.git/refs
    [ -f "$BACH_CLONE_SRC/packed-refs" ] && cp "$BACH_CLONE_SRC/packed-refs" /work/.git/packed-refs
    cp "$BACH_CLONE_SRC/HEAD" /work/.git/HEAD
    chown -R agent:agent /work/.git
    runuser -u agent -- git -C /work reset --hard --quiet
    if [ -n "${BACH_CLONE_ORIGIN:-}" ]; then
        runuser -u agent -- git -C /work remote add origin "$BACH_CLONE_ORIGIN"
    fi
    # Snapshot starting branch tips so the exit guard ignores pre-existing
    # unpushed commits and only flags work done during this session. Run the
    # redirect inside the runuser too so the file lands agent-owned.
    runuser -u agent -- bash -c \
        'git -C /work for-each-ref --format="%(objectname)" refs/heads > /work/.git/bach-start-heads' \
        || true
fi

# Overlay-mode caches: host base bind-mounted at /bach-cache/<slug>. Warm
# the per-session working copy at /work/<path> from base at start; promote
# back on exit (last-writer-wins on concurrent sessions).
OVERLAY_TARGETS=()
if [ -s /bach-runtime/cache-manifest ]; then
    while IFS=$'\t' read -r slug target; do
        [ -z "$slug" ] && continue
        [ -d "/bach-cache/$slug" ] || continue
        mkdir -p "$target"
        rsync -a "/bach-cache/$slug/" "$target/" 2>/dev/null || true
        chown -R agent:agent "$target"
        OVERLAY_TARGETS+=("$slug|$target")
    done < /bach-runtime/cache-manifest
fi

cleanup_caches() {
    if [ "${#OVERLAY_TARGETS[@]}" -eq 0 ]; then
        return
    fi
    for entry in "${OVERLAY_TARGETS[@]}"; do
        IFS='|' read -r slug target <<< "$entry"
        rsync -a "$target/" "/bach-cache/$slug/" 2>/dev/null || true
    done
}
trap cleanup_caches EXIT

status=0
runuser --preserve-environment -u agent -- /usr/local/bin/bach-session "$@" || status=$?
exit $status

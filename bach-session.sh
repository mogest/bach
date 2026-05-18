#!/bin/bash
# bach-session — wraps the primary session command. If the command exits
# while /work has uncommitted or unpushed work (clone mode only), prompts
# before tearing down the --rm container. Declining drops the user back
# into a fallback shell so they can commit/push; the check repeats on its
# exit. Works the same regardless of which shell the inner command is, so
# this hooks into Ctrl-D etc. for free (bash, fish, …).

set +e

# Opt-in via BACH_GUARD=1. Anything else (one-shot `bach run`, direct
# `bach c <args>`, non-interactive runs) execs through unchanged.
if [ -z "${BACH_GUARD:-}" ] || [ ! -t 0 ]; then
    exec "$@"
fi

# Fallback shell after the user chooses to stay. Defaults to `bash -l`;
# .bach.toml can swap it via BACH_RETURN_SHELL (e.g. "fish -l").
read -r -a _return_shell <<< "${BACH_RETURN_SHELL:-bash -l}"

_unsaved_summary() {
    # Bind mode: host owns /work, exit is harmless.
    [ -d /host-git ]  || return 1
    [ -d /work/.git ] || return 1
    local dirty_n unpushed_n

    # Exclude commits reachable from any branch tip that existed at session
    # start — those pre-existing "unpushed" commits live on the host and
    # aren't at risk if we exit. Only count commits made during this session.
    local start_excludes=()
    if [ -r /work/.git/bach-start-heads ]; then
        local sha
        while IFS= read -r sha; do
            [ -n "$sha" ] && start_excludes+=( "^$sha" )
        done < /work/.git/bach-start-heads
    fi

    dirty_n=$(git -C /work status --porcelain 2>/dev/null | wc -l | tr -d ' ')
    # Order matters: ^<sha> excludes must come BEFORE `--not --remotes`, otherwise
    # git silently drops them (the --not toggle suppresses the ^).
    unpushed_n=$(git -C /work rev-list --count --branches "${start_excludes[@]}" --not --remotes 2>/dev/null)
    unpushed_n=${unpushed_n:-0}

    [ "${dirty_n:-0}" -eq 0 ] && [ "${unpushed_n:-0}" -eq 0 ] && return 1

    printf '\nbach: unsaved work in /work:\n' >&2
    [ "${dirty_n:-0}"    -gt 0 ] && printf '  uncommitted changes: %s file(s)\n'   "$dirty_n"    >&2
    [ "${unpushed_n:-0}" -gt 0 ] && printf '  unpushed commits this session: %s\n' "$unpushed_n" >&2
    return 0
}

_prompt_exit() {
    local ans
    if ! read -r -p 'Exit anyway? [y/N] ' ans </dev/tty; then
        return 1
    fi
    case "$ans" in
        y|Y|yes|YES) return 0 ;;
        *) return 1 ;;
    esac
}

"$@"
rc=$?

while _unsaved_summary; do
    if _prompt_exit; then
        break
    fi
    printf 'bach: dropping into %s — commit/push, then exit again to leave.\n' "${_return_shell[0]}" >&2
    "${_return_shell[@]}"
    rc=$?
done

exit "$rc"

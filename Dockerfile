FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        fd-find \
        gh \
        git \
        gnupg \
        inotify-tools \
        jq \
        less \
        locales \
        openssh-client \
        procps \
        ripgrep \
        rsync \
        sudo \
        tini \
        unzip \
        util-linux \
        xz-utils \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && sed -i '/en_US.UTF-8/s/^# //' /etc/locale.gen \
    && locale-gen en_US.UTF-8 \
    && rm -rf /var/lib/apt/lists/*

ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8

# Wire git to use gh as the credential helper for github.com so `git push`
# inside the sandbox uses the same token gh does (sourced from GH_TOKEN env
# the wrapper sets). System-level config so it applies to any user.
RUN git config --system 'credential.https://github.com.helper' '!gh auth git-credential' \
 && git config --system 'credential.https://gist.github.com.helper' '!gh auth git-credential'

RUN useradd --create-home --shell /bin/bash --uid 1000 agent \
    && mkdir -p /work /certs \
    && chown agent:agent /work

# Install claude + mise as the agent user so they land in /home/agent.
USER agent
WORKDIR /home/agent
RUN curl -fsSL https://claude.ai/install.sh | bash
RUN curl -fsSL https://mise.run | sh

# Set PATH for agent. Affects the dropped-priv shell launched by entrypoint.
ENV PATH="/home/agent/.local/bin:/home/agent/.local/share/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
ENV MISE_TRUSTED_CONFIG_PATHS="/work"

RUN cat >> /home/agent/.bashrc <<'EOF'

# bach: ensure user bin + mise shims are on PATH even after /etc/profile reset.
export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"
eval "$(mise activate bash)"
EOF

# Switch back to root for entrypoint; entrypoint drops to agent after setup.
USER root
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY bach-session.sh /usr/local/bin/bach-session
COPY pbcopy /usr/local/bin/pbcopy
COPY bach-rcfile /etc/bach-rcfile
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/bach-session /usr/local/bin/pbcopy \
 && chmod 644 /etc/bach-rcfile

WORKDIR /work

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["bash"]

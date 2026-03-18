FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Install system deps, Node.js 20, and Claude Code in a single layer to keep
# the image small.  ca-certificates is required for curl HTTPS connections.
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip \
    docker.io \
    iptables \
    curl \
    ca-certificates \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code

# Pre-install the full math toolkit so Claude has a capable Python environment
# for all math benchmarks (optimization, signal processing, geometry, etc.).
RUN pip3 install --no-cache-dir \
    numpy scipy sympy \
    jax optax \
    torch --index-url https://download.pytorch.org/whl/cpu \
    scikit-learn numba \
    pandas matplotlib plotly \
    networkx cvxpy autograd pymoo \
    PyWavelets

# Non-root user for running Claude Code (it refuses --dangerously-skip-permissions
# as root). Added to the docker group so it can talk to dockerd in DinD mode.
RUN useradd -m -s /bin/bash -G docker claude

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /workspace
ENTRYPOINT ["/entrypoint.sh"]

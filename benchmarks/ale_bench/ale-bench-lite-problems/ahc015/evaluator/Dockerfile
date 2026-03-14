FROM python:3.12-slim

RUN apt-get update && apt-get install -y \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /benchmark

# ale-bench and ale-bench-eval must be installed before building.
# Copy wheel files into this directory, or install from your private index.
# Uncomment the next two lines once the wheels are available:
# COPY ale_bench*.whl /tmp/
# RUN pip install --no-cache-dir /tmp/ale_bench*.whl

COPY . .
RUN chmod +x evaluate.sh
ENTRYPOINT ["./evaluate.sh"]

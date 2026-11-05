FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install --no-install-recommends -y python3 python3-psycopg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY api-credential-bridge.py /opt/modelguard/api-credential-bridge.py
USER 1000:1000
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    MODEL_GUARD_DATA_DIR=/var/lib/modelguard \
    MODEL_GUARD_DB_MODE=direct \
    MODEL_GUARD_DB_HOST=sub2api-postgres \
    MODEL_GUARD_DB_USER=modelguard_bridge \
    MODEL_GUARD_DB_NAME=sub2api \
    MODEL_GUARD_DB_PASSFILE=/etc/modelguard/pgpass
ENTRYPOINT ["python3", "/opt/modelguard/api-credential-bridge.py"]

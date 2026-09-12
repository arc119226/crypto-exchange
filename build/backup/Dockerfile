# The backup sidecar (docs/runbooks/backup-restore.md): the stock Postgres
# client tools of the same major as the server (pg_dump, pg_restore, psql)
# plus MinIO's mc for the S3-compatible store. scripts/backup.sh is the
# entrypoint; scripts/restore-drill.sh rides along so the drill runs from
# the same container in CI and on the VM.
ARG POSTGRES_TAG=16.4
ARG MC_TAG=RELEASE.2025-08-13T08-35-41Z

# quay.io, not Docker Hub: MinIO deleted both minio/mc and minio/minio from
# Docker Hub, and this build started failing with "pull access denied,
# repository does not exist" on a pin that had never changed. quay.io/minio
# is MinIO's own registry and still serves this exact tag, so only the host
# moved. Do not shorten it back to minio/mc.
FROM quay.io/minio/mc:${MC_TAG} AS mc

FROM postgres:${POSTGRES_TAG}
COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY scripts/backup.sh scripts/restore-drill.sh /usr/local/bin/
COPY scripts/sql/restore-checks.sql /usr/local/share/exchange/restore-checks.sql
RUN chmod 0755 /usr/local/bin/backup.sh /usr/local/bin/restore-drill.sh /usr/local/bin/mc
ENV MC_CONFIG_DIR=/tmp/.mc
ENTRYPOINT ["/usr/local/bin/backup.sh"]
CMD ["run"]
LABEL org.opencontainers.image.source="https://github.com/arc119226/crypto-exchange" \
      org.opencontainers.image.title="crypto-exchange-backup"

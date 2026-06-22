#!/usr/bin/env bash
# Portable Postgres backup for Pulse Pager.
#
# Produces an architecture-independent logical dump (cluster roles + the database)
# that restores on ANY machine and ANY Postgres version >= the source. Use this to
# move data off the Pi or to take routine backups.
#
# Do NOT copy the raw data directory to another computer instead: the Pi is 32-bit
# ARM and Postgres's on-disk format is not portable across CPU architecture, so a
# file-level copy will not load on a 64-bit machine. This logical dump is the
# portable path.
#
# Env overrides:
#   PULSE_DB_NAME     database name (default: pulse)
#   PULSE_BACKUP_DIR  where to write backups (default: /var/backups/pulse)
#   PULSE_BACKUP_KEEP how many timestamped backups to keep (default: 14)
#   PULSE_PG_SUPERUSER  OS user that owns the cluster (default: postgres)
set -euo pipefail

DB="${PULSE_DB_NAME:-pulse}"
BACKUP_DIR="${PULSE_BACKUP_DIR:-/var/backups/pulse}"
KEEP="${PULSE_BACKUP_KEEP:-14}"
SUPERUSER="${PULSE_PG_SUPERUSER:-postgres}"

# Run a Postgres tool as the cluster superuser (peer auth on a default apt install).
run() { sudo -u "$SUPERUSER" "$@"; }

ts="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${BACKUP_DIR}/${ts}"
mkdir -p "$dest"

# 1) Cluster roles (pulse, pulse_app) with their attributes and password hashes.
#    Restored first on the target because the database's RLS policies and grants
#    reference the pulse_app role.
run pg_dumpall --roles-only > "${dest}/roles.sql"

# 2) The database itself: schema + data + RLS policies, custom format (compressed).
run pg_dump --format=custom -d "$DB" > "${dest}/${DB}.dump"

# Integrity manifest so a half-copied or bit-rotted backup is caught on restore.
( cd "$dest" && sha256sum "roles.sql" "${DB}.dump" > SHA256SUMS )

echo "backup written: ${dest}"
du -sh "$dest" | awk '{print "size: " $1}'

# Prune: keep the newest $KEEP backup dirs.
if [ "$KEEP" -gt 0 ]; then
  ls -1dt "${BACKUP_DIR}"/*/ 2>/dev/null | tail -n +"$((KEEP + 1))" | xargs -r rm -rf
fi
echo "retained newest ${KEEP} backups in ${BACKUP_DIR}"

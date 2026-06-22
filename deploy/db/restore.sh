#!/usr/bin/env bash
# Restore a Pulse Pager backup (produced by backup.sh) onto THIS machine's Postgres.
# Works on any CPU architecture and any Postgres version >= the source, so this is
# how you move the data from the Pi to another computer.
#
# Usage: restore.sh <backup-dir>
#   e.g. restore.sh /var/backups/pulse/20260622T033000Z
#
# WARNING: this drops and recreates the target database. Set PULSE_RESTORE_FORCE=1
# to skip the confirmation prompt (for scripted use).
#
# Env overrides: PULSE_DB_NAME (default pulse), PULSE_PG_SUPERUSER (default postgres)
set -euo pipefail

SRC="${1:?usage: restore.sh <backup-dir>}"
DB="${PULSE_DB_NAME:-pulse}"
SUPERUSER="${PULSE_PG_SUPERUSER:-postgres}"
run() { sudo -u "$SUPERUSER" "$@"; }

# Verify integrity before touching the live database.
( cd "$SRC" && sha256sum -c SHA256SUMS )

if [ "${PULSE_RESTORE_FORCE:-0}" != "1" ]; then
  echo "WARNING: this drops and recreates database '${DB}' on this machine."
  read -rp "type 'yes' to continue: " ans
  [ "$ans" = "yes" ] || { echo "aborted"; exit 1; }
fi

# 1) Roles first: the database's RLS policies and grants reference pulse_app, so the
#    roles must exist before the restore. psql continues past "role already exists".
run psql -d postgres -f "${SRC}/roles.sql"

# 2) Recreate the database owned by pulse and restore schema + data + RLS policies.
run dropdb --if-exists "$DB"
run createdb -O pulse "$DB"
run pg_restore -d "$DB" "${SRC}/${DB}.dump"

echo "restored '${DB}' on this machine from ${SRC}"

# Database data: persistence and moving it

This covers two things for the single-node deployment: keeping Postgres data across
restarts, and moving it to another computer.

## Where the data lives (survives restarts)

The native (apt) Postgres on the Pi stores its data on disk at
`/var/lib/postgresql/<version>/main`. That is durable across service restarts and
reboots out of the box. Nothing extra is needed for restart survival.

Two cautions on a Raspberry Pi:

- **SD cards corrupt on power loss.** Use clean shutdowns where you can, and do not
  treat the SD card as safe storage.
- **Back it up** (see "Routine backups"). Durable storage is not a backup.

## Moving the data to another computer

Do **not** copy the raw data directory to another machine. The Pi is 32-bit ARM and
Postgres's on-disk format is not portable across CPU architecture, so a file-level
copy will not load on a 64-bit box. Use a logical dump instead, which is portable
across any architecture and any Postgres version greater than or equal to the source
(Pi to x86 VPS, to an arm64 box, or to a managed cloud Postgres).

Take a backup on the Pi:

```sh
deploy/db/backup.sh          # writes /var/backups/pulse/<timestamp>/
```

Each backup directory holds:
- `roles.sql`   the cluster roles (`pulse`, `pulse_app`) with their attributes
- `pulse.dump`  the database: schema + data + RLS policies (custom format)
- `SHA256SUMS`  integrity manifest

Copy that directory to the new machine and restore it there (Postgres must be
installed):

```sh
scp -r pi:/var/backups/pulse/20260622T033000Z ./backup
deploy/db/restore.sh ./backup
```

`restore.sh` recreates the roles, then drops and recreates the `pulse` database and
restores into it. It prompts before dropping; set `PULSE_RESTORE_FORCE=1` to skip.

## Routine backups

`deploy/systemd/pulse-db-backup.{service,timer}` run `backup.sh` daily at 03:30 and
keep the newest 14. Install them:

```sh
sudo cp deploy/systemd/pulse-db-backup.* /etc/systemd/system/
sudo systemctl enable --now pulse-db-backup.timer
systemctl list-timers pulse-db-backup.timer
```

Copy backups off the Pi periodically: a backup that only lives on the same SD card
as the data is not real safety.

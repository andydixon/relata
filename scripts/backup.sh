#!/usr/bin/env bash
#
# Back up Relata: MongoDB, the uploaded originals, and optionally the Qdrant
# vectors.
#
# MongoDB and the upload directory must be captured together — an item row that
# refers to a file missing from the backup is a broken record. Qdrant is derived
# data and can be rebuilt by re-embedding, so its snapshot is a convenience.
#
#   ./scripts/backup.sh [destination-directory]
#
# Stop Relata first for a consistent snapshot. The workers shut down gracefully:
# in-flight jobs drain, and any that do not finish keep their leases and return
# to the queue on restart.

set -euo pipefail

# Backups contain the complete knowledge store and may include personal data.
# Do not let a permissive caller umask create group- or world-readable archives.
umask 077

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="${1:-backups}/${STAMP}"
COMPOSE="${COMPOSE:-docker compose}"
UPLOAD_DIR="${RELATA_UPLOAD_DIR:-./data/uploads}"
MONGO_DB="${RELATA_MONGO_DATABASE:-relata}"
QDRANT_URL="${RELATA_QDRANT_URL:-http://127.0.0.1:6333}"
QDRANT_COLLECTION="${RELATA_QDRANT_COLLECTION:-relata_chunks}"

mkdir -p "$DEST"
echo "==> Writing backup to $DEST"

# --- MongoDB: the irreplaceable part ---------------------------------------
echo "==> MongoDB ($MONGO_DB)"
$COMPOSE exec -T mongodb mongodump --db="$MONGO_DB" --archive --gzip \
  > "$DEST/relata-mongo.archive.gz"
echo "    $(du -h "$DEST/relata-mongo.archive.gz" | cut -f1)"

# --- Uploaded originals ------------------------------------------------------
if [ -d "$UPLOAD_DIR" ]; then
  echo "==> Uploaded originals ($UPLOAD_DIR)"
  tar -czf "$DEST/uploads.tar.gz" -C "$(dirname "$UPLOAD_DIR")" "$(basename "$UPLOAD_DIR")"
  echo "    $(du -h "$DEST/uploads.tar.gz" | cut -f1)"
else
  echo "==> No upload directory at $UPLOAD_DIR; skipping"
fi

# --- Qdrant: derived, so failure here is not fatal ---------------------------
echo "==> Qdrant snapshot (optional — vectors are rebuildable)"
if curl -fsS -X POST "$QDRANT_URL/collections/$QDRANT_COLLECTION/snapshots" \
     > "$DEST/qdrant-snapshot.json" 2>/dev/null; then
  echo "    snapshot requested; retrieve it from the Qdrant storage volume"
else
  echo "    Qdrant unreachable or snapshots disabled — re-embed after restoring instead"
  rm -f "$DEST/qdrant-snapshot.json"
fi

# --- Configuration, minus secrets -------------------------------------------
if [ -f .env ]; then
  echo "==> Configuration reference (secret values redacted)"
  # Redact by assignment rather than dropping matching lines: keeping the key
  # names is useful when restoring, and a comment that merely mentions
  # "password" is not a secret.
  sed -E 's/^([A-Za-z_][A-Za-z0-9_]*(PASSWORD|API_KEY|SECRET|TOKEN|URI))=.*/\1=[redacted]/' \
    .env > "$DEST/config-reference.env"
fi

cat > "$DEST/RESTORE.md" <<'EOF'
# Restoring this backup

    docker compose stop app

    docker compose exec -T mongodb mongorestore --archive --gzip --drop \
      < relata-mongo.archive.gz

    rm -rf ./data/uploads
    tar -xzf uploads.tar.gz -C ./data

    docker compose start app     # or: make dev

Then check the settings page. If "vectors stored" does not match "chunks in
MongoDB", use **Settings → Re-embed workspace** to rebuild them.

Full procedure: docs/BACKUP_AND_RESTORE.md
EOF

echo
echo "==> Backup complete: $DEST"
ls -lh "$DEST"

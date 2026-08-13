#!/usr/bin/env python3
"""Repair session-content blobs that contain invalid UTF-8.

The sai server stores large tool results (e.g. grep/read_file output) as
content-addressed blobs under ``<server-root>/data/sessions/<id>/blobs/sha256/``.
Older versions of the grep tool truncated long lines at a raw byte boundary,
which could split a multi-byte UTF-8 character and persist an invalid byte
sequence into a blob. When the session-content projector later validates the
blob with strict UTF-8 it fails, so the session cannot be opened in the UI
("subscription could not be opened").

This script scans every session database, finds text blobs whose contents are
not valid UTF-8, replaces the invalid byte sequences with U+FFFD, writes the
fixed content under a *new* content-addressed blob file, and updates every
item that referenced the old hash to point at the new hash / size.

Usage:
    python scripts/fix_bad_blobs.py                      # uses default server root
    python scripts/fix_bad_blobs.py --root C:\\path\\to\\sai-data
    python scripts/fix_bad_blobs.py --root ... --dry-run       # report only
    python scripts/fix_bad_blobs.py --root ... --session 20260804...
    python scripts/fix_bad_blobs.py --root ... --backup-dir C:\\backups

Notes:
    * Only text blobs (media_type starting with "text/") are validated and
      repaired. Binary attachments (images etc.) are left untouched.
    * Backs up every modified session.db to <session>/session.db.bak-utf8fix
      (or --backup-dir if given) before changing it.
    * The script is safe to run while the server is running: it writes new
      blob files and updates the DB in place; the running process re-reads
      blobs from disk on the next open. Restart is not required for the data
      fix (the code-side UTF-8 boundary fix requires a rebuild/restart).
"""

import argparse
import hashlib
import json
import os
import shutil
import sqlite3
import sys


def find_sessions(sessions_dir):
    """Yield (session_id, db_path, blobs_root) for every session directory."""
    if not os.path.isdir(sessions_dir):
        return
    for name in sorted(os.listdir(sessions_dir)):
        session_dir = os.path.join(sessions_dir, name)
        db_path = os.path.join(session_dir, "session.db")
        if os.path.isdir(session_dir) and os.path.isfile(db_path):
            yield name, db_path, os.path.join(session_dir, "blobs", "sha256")


def collect_blob_refs(db_path):
    """Return {hash: {"media": str, "items": [item_id, ...], "size": int}}."""
    refs = {}
    try:
        db = sqlite3.connect(db_path)
    except sqlite3.Error as exc:
        print(f"  !! cannot open db: {exc}", file=sys.stderr)
        return refs
    try:
        for (payload,) in db.execute("SELECT payload FROM items"):
            try:
                item = json.loads(payload)
            except (ValueError, TypeError):
                continue
            content = item.get("content")
            if not isinstance(content, dict):
                continue
            blob = content.get("blob")
            if not isinstance(blob, dict):
                continue
            h = blob.get("hash", "")
            if not h:
                continue
            entry = refs.setdefault(h, {"media": blob.get("media_type", ""), "items": [], "size": blob.get("size_bytes")})
            entry["items"].append(item.get("id"))
    except sqlite3.Error as exc:
        print(f"  !! query failed: {exc}", file=sys.stderr)
    finally:
        db.close()
    return refs


def is_valid_utf8(data):
    try:
        data.decode("utf-8")
        return True
    except UnicodeDecodeError:
        return False


def fix_content(raw):
    """Return (new_bytes, changed) replacing invalid UTF-8 with U+FFFD."""
    fixed = raw.decode("utf-8", errors="replace").encode("utf-8")
    return fixed, fixed != raw


def blob_path(blobs_root, h):
    return os.path.join(blobs_root, h[:2], h + ".data")


def write_blob(blobs_root, h, content):
    """Write content under a new content-addressed path; return new hash."""
    new_hash = hashlib.sha256(content).hexdigest()
    path = blob_path(blobs_root, new_hash)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(content)
    return new_hash


def update_item_blob(db, item_id, new_hash, new_size):
    """Rewrite one item row's content.blob hash/size_bytes."""
    cur = db.execute("SELECT payload FROM items WHERE id = ?", (item_id,))
    row = cur.fetchone()
    if row is None:
        return False
    try:
        item = json.loads(row[0])
    except (ValueError, TypeError):
        return False
    content = item.get("content")
    if not isinstance(content, dict):
        return False
    blob = content.get("blob")
    if not isinstance(blob, dict):
        return False
    blob["hash"] = new_hash
    blob["size_bytes"] = new_size
    payload = json.dumps(item, ensure_ascii=False, separators=(",", ":"))
    db.execute("UPDATE items SET payload = ? WHERE id = ?", (payload, item_id))
    return True


def backup_db(db_path, backup_dir):
    if backup_dir:
        os.makedirs(backup_dir, exist_ok=True)
        dest = os.path.join(backup_dir, os.path.basename(os.path.dirname(db_path)) + ".session.db.bak-utf8fix")
    else:
        dest = db_path + ".bak-utf8fix"
    shutil.copy2(db_path, dest)
    return dest


def repair_session(session_id, db_path, blobs_root, dry_run, backup_dir):
    refs = collect_blob_refs(db_path)
    if not refs:
        return 0, 0
    bad = {}
    for h, info in refs.items():
        if not info["media"].startswith("text/"):
            continue
        path = blob_path(blobs_root, h)
        if not os.path.isfile(path):
            bad[h] = ("missing", info)
            continue
        with open(path, "rb") as f:
            raw = f.read()
        if not is_valid_utf8(raw):
            bad[h] = ("invalid", info, raw)
    if not bad:
        return 0, 0

    print(f"SESSION {session_id}: {len(bad)} blob(s) to repair")
    if dry_run:
        for h, what in bad.items():
            reason = what[0]
            print(f"  - {h[:16]}... ({reason}) refs={len(what[1]['items'])}")
        return 0, len(bad)

    backup = backup_db(db_path, backup_dir)
    print(f"  backup -> {backup}")

    db = sqlite3.connect(db_path)
    db.execute("BEGIN")
    fixed_count = 0
    try:
        for h, what in bad.items():
            reason = what[0]
            if reason == "missing":
                print(f"  !! {h[:16]}... file missing; cannot repair")
                continue
            info, raw = what[1], what[2]
            fixed, changed = fix_content(raw)
            if not changed:
                continue
            new_hash = write_blob(blobs_root, h, fixed)
            for item_id in info["items"]:
                if not update_item_blob(db, item_id, new_hash, len(fixed)):
                    print(f"  !! could not update item {item_id} for {h[:16]}...")
            fixed_count += 1
            print(f"  - {h[:16]}... -> {new_hash[:16]}... (size {len(raw)}->{len(fixed)}, refs={len(info['items'])})")
        db.commit()
    except Exception:
        db.rollback()
        raise
    finally:
        db.close()
    return fixed_count, len(bad)


def main():
    parser = argparse.ArgumentParser(description="Repair session blobs with invalid UTF-8")
    parser.add_argument("--root", default=None,
                        help="sai server root (default: OS user config dir + basename 'sai')")
    parser.add_argument("--session", default=None,
                        help="only repair this session directory name")
    parser.add_argument("--dry-run", action="store_true",
                        help="scan and report without changing anything")
    parser.add_argument("--backup-dir", default=None,
                        help="directory for database backups (default: next to each session.db)")
    parser.add_argument("--basename", default="sai",
                        help="application basename used to derive the default root")
    args = parser.parse_args()

    if args.root:
        root = args.root
    else:
        config_home = os.environ.get("APPDATA") or os.environ.get("XDG_CONFIG_HOME") or os.path.expanduser("~")
        if sys.platform == "win32" and os.environ.get("APPDATA"):
            root = os.path.join(os.environ["APPDATA"], args.basename)
        else:
            root = os.path.join(config_home, args.basename)
    sessions_dir = os.path.join(root, "data", "sessions")
    if not os.path.isdir(sessions_dir):
        print(f"session store not found: {sessions_dir}", file=sys.stderr)
        return 1

    print(f"server root: {root}")
    print(f"session store: {sessions_dir}")
    print(f"mode: {'DRY RUN' if args.dry_run else 'REPAIR'}")
    print()

    total_bad = 0
    total_fixed = 0
    sessions_scanned = 0
    for session_id, db_path, blobs_root in find_sessions(sessions_dir):
        if args.session and session_id != args.session:
            continue
        sessions_scanned += 1
        fixed, bad = repair_session(session_id, db_path, blobs_root, args.dry_run, args.backup_dir)
        total_fixed += fixed
        total_bad += bad

    print()
    print(f"scanned sessions: {sessions_scanned}")
    print(f"bad blobs found: {total_bad}")
    print(f"blobs fixed: {total_fixed}")
    if args.dry_run:
        print("(dry run - no changes were made)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
package minitest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const (
	litestreamLabel = "org.omarchy-mac.lab.litestream"
	backupLabel     = "org.omarchy-mac.lab.backup"
	backupKeys      = "var/lab/secrets/backup"
	resticCopy      = "usr/local/lab/tools/current/restic/bin/restic"
)

// restic is played by a script: a repository is a folder, a snapshot a copy
// of the files restic was given, less what --exclude names. It writes down
// its arguments and, apart from them, what its environment opened the
// repository with. A pattern that starts with / holds from the root, any
// other names a file or folder at any depth, as restic reads them; unlike
// restic, * here also crosses a /.
const resticStub = `#!/bin/bash
{
  printf 'restic'
  printf ' %s' "$@"
  printf ' [repository=%s password-file=%s rest=%s:%s aws=%s:%s cacert=%s cache=%s]\n' \
    "${RESTIC_REPOSITORY:-file:${RESTIC_REPOSITORY_FILE:-}}" "${RESTIC_PASSWORD_FILE:-}" \
    "${RESTIC_REST_USERNAME:-}" "${RESTIC_REST_PASSWORD:-}" "${AWS_ACCESS_KEY_ID:-}" "${AWS_SECRET_ACCESS_KEY:-}" \
    "${RESTIC_CACERT:-}" "${RESTIC_CACHE_DIR:-}"
} >> "$STUB_LOG"
repo="${RESTIC_REPOSITORY:-$(cat "$RESTIC_REPOSITORY_FILE")}"
case "$repo" in
  rest:https://lab-backup:8000/mini/) name=house ;;
  s3:https://*) name=offsite ;;
  *) echo "Fatal: unable to open repository at $repo" >&2; exit 1 ;;
esac
[[ -s ${RESTIC_PASSWORD_FILE:-} ]] || { echo "Fatal: no password" >&2; exit 1; }
dir="$STUB_STATE/restic/$name"
cmd="$1"
shift
if grep -qx "$name $cmd" "$STUB_STATE/restic.fails" 2>/dev/null; then
  echo "Fatal: $cmd was made to fail" >&2
  exit 1
fi
case "$cmd" in
  backup)
    excludes=() paths=()
    while (( $# )); do
      case "$1" in
        --exclude) excludes+=("$2"); shift ;;
        --host) shift ;;
        -*) echo "Fatal: unknown flag $1" >&2; exit 1 ;;
        *) paths+=("$1") ;;
      esac
      shift
    done
    id="$(printf '%08d' $(( $(ls "$dir/snapshots" 2>/dev/null | wc -l) + 1 )))"
    snap="$dir/snapshots/$id"
    mkdir -p "$snap/files"
    date -u +%Y-%m-%dT%H:%M:%S.000000000Z > "$snap/time"
    for p in "${paths[@]}"; do
      while IFS= read -r f; do
        skip=0
        for pat in "${excludes[@]}"; do
          if [[ $pat == /* ]]; then
            if [[ $f == $pat || $f == $pat/* ]]; then skip=1; fi
          else
            IFS=/ read -ra parts <<<"$f"
            for part in "${parts[@]}"; do
              if [[ $part == $pat ]]; then skip=1; fi
            done
          fi
        done
        (( skip )) && continue
        mkdir -p "$snap/files$(dirname "$f")"
        cp "$f" "$snap/files$f"
      done < <(find "$p" -type f)
    done
    echo "snapshot $id saved" ;;
  snapshots)
    printf '['
    sep=""
    for s in "$dir"/snapshots/*/; do
      [[ -d $s ]] || continue
      printf '%s{"id":"%s","short_id":"%s","time":"%s"}' "$sep" "$(basename "$s")" "$(basename "$s")" "$(<"$s/time")"
      sep=,
    done
    printf ']\n' ;;
  forget)
    for a in "$@"; do
      if [[ $a != -* && -d $dir/snapshots/$a ]]; then rm -rf "$dir/snapshots/$a"; fi
    done ;;
  restore)
    id="$1"
    shift
    [[ $id != latest ]] || id="$(ls "$dir/snapshots" | tail -n 1)"
    target="" includes=()
    while (( $# )); do
      case "$1" in
        --target) target="$2"; shift ;;
        --include) includes+=("$2"); shift ;;
        *) echo "Fatal: unknown flag $1" >&2; exit 1 ;;
      esac
      shift
    done
    [[ -n $target && -d $dir/snapshots/$id ]] || { echo "Fatal: no snapshot" >&2; exit 1; }
    mkdir -p "$target"
    if (( ${#includes[@]} == 0 )); then
      cp -R "$dir/snapshots/$id/files/." "$target"
    else
      for p in "${includes[@]}"; do
        [[ -e $dir/snapshots/$id/files$p ]] || continue
        mkdir -p "$target$(dirname "$p")"
        cp -R "$dir/snapshots/$id/files$p" "$target$p"
      done
    fi ;;
  ls)
    id="" folders=()
    for a in "$@"; do
      case "$a" in
        --json) ;;
        -*) echo "Fatal: unknown flag $a" >&2; exit 1 ;;
        *) if [[ -z $id ]]; then id="$a"; else folders+=("$a"); fi ;;
      esac
    done
    [[ $id != latest ]] || id="$(ls "$dir/snapshots" | tail -n 1)"
    [[ -d $dir/snapshots/$id ]] || { echo "Fatal: no snapshot" >&2; exit 1; }
    printf '{"time":"%s","paths":["/"],"hostname":"lab-mini","id":"%s","struct_type":"snapshot"}\n' "$(<"$dir/snapshots/$id/time")" "$id"
    for p in "${folders[@]}"; do
      for f in "$dir/snapshots/$id/files$p"/*; do
        [[ -e $f ]] || continue
        kind=file
        [[ ! -d $f ]] || kind=dir
        printf '{"name":"%s","type":"%s","path":"%s","struct_type":"node"}\n' "$(basename "$f")" "$kind" "$p/$(basename "$f")"
      done
    done ;;
esac
`

// sftp reads its batch as the real one does: "-get" goes on when a file is
// not there. The guest's files are in the test's state folder.
const sftpStub = `#!/bin/bash
echo "sftp $*" >> "$STUB_LOG"
[[ ! -f $STUB_STATE/sftp.fails ]] || exit 255
while read -r verb from to; do
  cp "$STUB_STATE/guest$from" "$to" 2>/dev/null || [[ $verb == -get ]] || exit 1
done
`

var houseSecrets = map[string]string{
	"restic-password":     "the-minis-restic-key\n",
	"rest-password":       "the-rest-servers-password\n",
	"guest.crt":           "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
	"litestream-sftp-key": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n-----END OPENSSH PRIVATE KEY-----\n",
	"host-key":            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuestGuestGuestGuestGuestGuestGuestGuestGuestG\n",
}

var offsiteSecrets = map[string]string{
	"r2-repository":        "s3:https://0123456789abcdef.r2.cloudflarestorage.com/lab-backup\n",
	"r2-access-key-id":     "the-r2-key-id\n",
	"r2-secret-access-key": "the-r2-secret\n",
}

// backedUp is a mini with a data directory labd has open, the keys of both
// targets, restic in the root-owned copy and a guest whose jobs ran.
type backedUp struct {
	*mini
	state string
	labd  *sql.DB
}

func newBackedUp(t *testing.T) *backedUp {
	t.Helper()
	need(t, "sqlite3", "jq")
	m := newMini(t)
	b := &backedUp{mini: m, state: filepath.Join(filepath.Dir(m.root), "state")}
	write(t, m.path(resticCopy), resticStub, 0o755)
	write(t, m.bin+"/sftp", sftpStub, 0o755)
	for _, d := range []string{"var/lab/run", "var/lab/log"} {
		if err := os.MkdirAll(m.path(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, set := range []map[string]string{houseSecrets, offsiteSecrets} {
		for name, text := range set {
			write(t, m.path(backupKeys+"/"+name), text, 0o400)
		}
	}
	write(t, m.path(backupKeys+"/known_hosts"), "lab-backup "+houseSecrets["host-key"], 0o400)

	data := m.path("var/lab/data")
	for _, name := range []string{"labd.db", "reports.db"} {
		db, err := sql.Open("sqlite", filepath.Join(data, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		os.MkdirAll(data, 0o700)
		for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE t (x TEXT)", "INSERT INTO t VALUES ('in " + name + "')"} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", name, q, err)
			}
		}
		if name == "labd.db" {
			b.labd = db
		}
		if !exists(filepath.Join(data, name+"-wal")) {
			t.Fatalf("%s has no -wal file: the test's labd is not holding it open as labd does", name)
		}
	}
	write(t, data+"/admin.token", "first-token\n", 0o600)
	write(t, data+"/artifacts/ab/kernel.img", "a kernel\n", 0o600)
	write(t, data+"/artifacts/.upload-123", "half an upload\n", 0o600)
	write(t, data+"/builds/7/.upload-9", "half another\n", 0o600)
	write(t, data+"/callers/2026-11-14.log", "203.0.113.9 GET /\n", 0o600)
	write(t, data+"/.labd.db-litestream/ltx/0/1.ltx", "litestream's own\n", 0o600)
	write(t, data+"/.reports.db-litestream/ltx/0/1.ltx", "litestream's own\n", 0o600)
	b.guestSays("mirror", "ok", time.Now(), "")
	b.guestSays("prune", "ok", time.Now(), "")
	b.guestSays("trim", "ok", time.Now(), "")
	return b
}

func (b *backedUp) guestSays(job, state string, at time.Time, reason string) {
	b.t.Helper()
	write(b.t, filepath.Join(b.state, "guest/status", job), fmt.Sprintf("%s %d %s %s\n", state, at.Unix(), at.UTC().Format(time.RFC3339), reason), 0o644)
}

// carried lists what one snapshot of a target holds, as paths under the data directory.
func (b *backedUp) carried(target, id string) []string {
	b.t.Helper()
	root := filepath.Join(b.state, "restic", target, "snapshots", id, "files")
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, strings.TrimPrefix(p, filepath.Join(root, b.path("var/lab/data"))+"/"))
		}
		return nil
	})
	return out
}

func (b *backedUp) snapshots(target string) []string {
	entries, _ := os.ReadDir(filepath.Join(b.state, "restic", target, "snapshots"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (b *backedUp) told() string { return read(b.t, b.path("var/lab/log/notify.log")) }

func rows(t *testing.T, file string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+file+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := db.Query("SELECT x FROM t ORDER BY rowid")
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var x string
		r.Scan(&x)
		out = append(out, x)
	}
	return out
}

func same(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

func TestTheNightlyRunCarriesACopyOfEachDatabaseAndLeavesTheLiveFilesOut(t *testing.T) {
	b := newBackedUp(t)
	// Written after the last checkpoint: it is in the -wal file only.
	if _, err := b.labd.Exec("INSERT INTO t VALUES ('a minute ago')"); err != nil {
		t.Fatal(err)
	}
	out := b.ok("backup")

	house := b.carried("house", "00000001")
	want := []string{"admin.token", "artifacts/ab/kernel.img", "snapshots/labd.db", "snapshots/reports.db"}
	if !same(house, want) {
		t.Errorf("the house copy carries\n  %s\nwant\n  %s\n%s", strings.Join(house, "\n  "), strings.Join(want, "\n  "), out)
	}
	offsite := b.carried("offsite", "00000001")
	if !same(offsite, []string{"snapshots/labd.db", "snapshots/reports.db"}) {
		t.Errorf("R2 is sent %v, want the two copies and nothing else", offsite)
	}

	// The copy is a whole database, with what was only in the -wal file.
	if got := rows(t, b.path("var/lab/data/snapshots/labd.db")); !same(got, []string{"in labd.db", "a minute ago"}) {
		t.Errorf("the copy of labd.db holds %v", got)
	}
	for _, f := range []string{"snapshots/labd.db-wal", "snapshots/labd.db-shm", "snapshots/.labd.db.new"} {
		if exists(b.path("var/lab/data/" + f)) {
			t.Errorf("%s was left behind", f)
		}
	}

	// Back from the house copy, to another folder.
	back := t.TempDir()
	b.ok("backup", "restic", "house", "restore", "latest", "--target", back)
	restored := filepath.Join(back, b.path("var/lab/data"))
	if got := rows(t, restored+"/snapshots/labd.db"); !same(got, []string{"in labd.db", "a minute ago"}) {
		t.Errorf("labd.db restored from the house copy holds %v", got)
	}
	if got := rows(t, restored+"/snapshots/reports.db"); !same(got, []string{"in reports.db"}) {
		t.Errorf("reports.db restored from the house copy holds %v", got)
	}
	if got := read(t, restored+"/artifacts/ab/kernel.img"); got != "a kernel\n" {
		t.Errorf("the artifact came back as %q", got)
	}
	if exists(restored + "/callers") {
		t.Error("the caller log came back from a backup")
	}

	if b.told() != "" {
		t.Errorf("a good run told the owner: %s", b.told())
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(read(t, b.path("var/lab/log/backup.ok")))); err != nil {
		t.Errorf("a good run did not write when it ended: %v", err)
	}
	if exists(b.path("var/lab/run/backup.lock")) {
		t.Error("the run left its lock")
	}
}

func resticCalls(b *backedUp) (house, offsite []string) {
	for _, line := range strings.Split(b.calls(), "\n") {
		switch {
		case !strings.HasPrefix(line, "restic "):
		case strings.Contains(line, "[repository=rest:https://lab-backup:8000/mini/ "):
			house = append(house, line)
		default:
			offsite = append(offsite, line)
		}
	}
	return house, offsite
}

func TestTheMiniDeletesNothingInTheHouseAndKeepsR2ToTheRetention(t *testing.T) {
	b := newBackedUp(t)
	// Two nights the keep rules would leave on R2 if nothing else ran: one
	// older than six months, one inside them.
	for id, age := range map[string]int{"00000001": 200, "00000002": 100} {
		at := time.Now().UTC().AddDate(0, 0, -age).Format("2006-01-02T15:04:05.000000000Z")
		write(t, filepath.Join(b.state, "restic/offsite/snapshots", id, "time"), at+"\n", 0o644)
	}
	b.ok("backup")

	house, offsite := resticCalls(b)
	if len(house) != 1 || !strings.HasPrefix(house[0], "restic backup ") {
		t.Errorf("the mini asked more of the house copy than one backup:\n%s", strings.Join(house, "\n"))
	}
	var kept, capped bool
	for _, line := range offsite {
		args := strings.SplitN(line, " [", 2)[0]
		if args == "restic forget --group-by host --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune" {
			kept = true
		}
		if args == "restic forget --prune 00000001" {
			capped = true
		}
	}
	if !kept {
		t.Errorf("R2 is not kept to 7 daily, 4 weekly, 6 monthly:\n%s", strings.Join(offsite, "\n"))
	}
	if !capped {
		t.Errorf("a snapshot 200 days old was not forgotten by name:\n%s", strings.Join(offsite, "\n"))
	}
	if got := b.snapshots("offsite"); !same(got, []string{"00000002", "00000003"}) {
		t.Errorf("R2 holds %v, want the one of 100 days ago and tonight's", got)
	}
}

func TestWhatOpensATargetIsNeverAnArgument(t *testing.T) {
	b := newBackedUp(t)
	out := b.ok("backup")
	secrets := []string{"the-minis-restic-key", "the-rest-servers-password", "the-r2-key-id", "the-r2-secret", "0123456789abcdef"}
	for _, line := range strings.Split(b.calls(), "\n") {
		args := strings.SplitN(line, " [", 2)[0]
		for _, s := range secrets {
			if strings.Contains(args, s) {
				t.Errorf("%q is an argument: %s", s, args)
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the run printed %q", s)
		}
	}
	keys := b.path(backupKeys)
	house, offsite := resticCalls(b)
	wantHouse := "[repository=rest:https://lab-backup:8000/mini/ password-file=" + keys + "/restic-password rest=mini:the-rest-servers-password aws=: cacert=" + keys + "/guest.crt cache=" + b.path("var/lab/cache/restic") + "]"
	for _, line := range house {
		if !strings.HasSuffix(line, wantHouse) {
			t.Errorf("the house copy is opened with\n  %s\nwant\n  %s", line, wantHouse)
		}
	}
	wantOffsite := "[repository=file:" + keys + "/r2-repository password-file=" + keys + "/restic-password rest=: aws=the-r2-key-id:the-r2-secret cacert= cache=" + b.path("var/lab/cache/restic") + "]"
	for _, line := range offsite {
		if !strings.HasSuffix(line, wantOffsite) {
			t.Errorf("R2 is opened with\n  %s\nwant\n  %s", line, wantOffsite)
		}
	}
	if len(house) == 0 || len(offsite) < 3 {
		t.Errorf("restic ran %d times for the house and %d for R2", len(house), len(offsite))
	}
	sftp := ""
	for _, line := range strings.Split(b.calls(), "\n") {
		if strings.HasPrefix(line, "sftp ") {
			sftp = line
		}
	}
	for _, want := range []string{"-i " + keys + "/litestream-sftp-key", "StrictHostKeyChecking=yes", "UserKnownHostsFile=" + keys + "/known_hosts", "BatchMode=yes", "litestream@lab-backup"} {
		if !strings.Contains(sftp, want) {
			t.Errorf("the guest is asked without %s: %s", want, sftp)
		}
	}
}

func TestAFailedBackupTellsTheOwnerAndGoesOn(t *testing.T) {
	b := newBackedUp(t)
	write(t, filepath.Join(b.state, "restic.fails"), "house backup\n", 0o644)
	out := b.refused("backup")
	if got := b.told(); !strings.Contains(got, " agents The nightly backup failed: the house copy: the data directory to lab-backup.") {
		t.Errorf("the owner is told %q\n%s", got, out)
	}
	if len(b.snapshots("offsite")) != 1 {
		t.Error("R2 got nothing because the house copy failed")
	}
	if exists(b.path("var/lab/log/backup.ok")) {
		t.Error("a failed run wrote that it went well")
	}

	// A database that cannot be copied: no copy of it is sent as tonight's.
	b = newBackedUp(t)
	b.ok("backup")
	for _, f := range []string{"reports.db", "reports.db-wal", "reports.db-shm"} {
		os.Remove(b.path("var/lab/data/" + f))
	}
	out = b.refused("backup")
	if !strings.Contains(b.told(), "the copy of reports.db") {
		t.Errorf("the owner is told %q\n%s", b.told(), out)
	}
	if got := b.carried("house", "00000002"); !same(got, []string{"admin.token", "artifacts/ab/kernel.img", "snapshots/labd.db"}) {
		t.Errorf("after reports.db could not be copied the house copy carries %v", got)
	}
}

func TestTheNightlyRunSaysWhenTheGuestsOwnJobsFailed(t *testing.T) {
	for name, c := range map[string]struct {
		set  func(b *backedUp)
		want string
	}{
		"the mirror failed": {
			func(b *backedUp) {
				b.guestSays("mirror", "failed", time.Now(), "git: could not reach `github.com`; $(reboot)")
			},
			"the owner's copy (the nightly mirror) failed on lab-backup: git: could not reach github.com reboot",
		},
		"the mirror stopped running": {
			func(b *backedUp) { b.guestSays("mirror", "ok", time.Now().Add(-40*time.Hour), "") },
			"the owner's copy (the nightly mirror) last ran more than 36 hours ago",
		},
		"pruning never ran": {
			func(b *backedUp) { os.Remove(filepath.Join(b.state, "guest/status/prune")) },
			"the pruning of the house copy has never run on lab-backup",
		},
		"pruning failed": {
			func(b *backedUp) { b.guestSays("prune", "failed", time.Now(), "restic forget") },
			"the pruning of the house copy failed on lab-backup: restic forget",
		},
		"the clean-up of Litestream's files never ran": {
			func(b *backedUp) { os.Remove(filepath.Join(b.state, "guest/status/trim")) },
			"the clean-up of Litestream's files has never run on lab-backup",
		},
		"the clean-up of Litestream's files failed": {
			func(b *backedUp) {
				b.guestSays("trim", "failed", time.Now(), "removing Litestream files older than six months")
			},
			"the clean-up of Litestream's files failed on lab-backup: removing Litestream files older than six months",
		},
		"the guest is away": {
			func(b *backedUp) { write(b.t, filepath.Join(b.state, "sftp.fails"), "", 0o644) },
			"lab-backup did not answer over SFTP, so its own jobs are not known",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBackedUp(t)
			c.set(b)
			out := b.refused("backup")
			if got := b.told(); !strings.Contains(got, c.want) || strings.Count(got, "\n") != 1 {
				t.Errorf("the owner is told %q, want one line with %q\n%s", got, c.want, out)
			}
		})
	}
	b := newBackedUp(t)
	b.guestSays("mirror", "ok", time.Now().Add(-30*time.Hour), "")
	b.ok("backup")
}

func TestTheBackupsDryRunSaysWhatWouldRunAndRunsNothing(t *testing.T) {
	b := newBackedUp(t)
	data := b.path("var/lab/data")
	out := b.ok("backup", "--dry-run")
	cut := time.Now().UTC().AddDate(0, 0, -183).Format("2006-01-02")
	want := strings.Join([]string{
		"backup: would copy " + data + "/labd.db to " + data + "/snapshots/labd.db with VACUUM INTO",
		"backup: would copy " + data + "/reports.db to " + data + "/snapshots/reports.db with VACUUM INTO",
		"backup: would run, for the house copy: the data directory to lab-backup: restic backup --host lab-mini" +
			" --exclude " + data + "/labd.db --exclude " + data + "/labd.db-* --exclude " + data + "/labd.db.restored --exclude " + data + "/.labd.db-litestream" +
			" --exclude " + data + "/reports.db --exclude " + data + "/reports.db-* --exclude " + data + "/reports.db.restored --exclude " + data + "/.reports.db-litestream" +
			" --exclude " + data + "/callers --exclude .upload-* " + data,
		"backup: would run, for the off-site copy: the databases' copies to R2: restic backup --host lab-mini " + data + "/snapshots",
		"backup: would run, for the off-site copy's retention: restic forget --group-by host --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune",
		"backup: would forget every off-site snapshot made before " + cut,
		"backup: would ask lab-backup how its pruning, its clean-up of Litestream's files and the owner's copy went",
		"backup: would ping the check backup",
		"backup: nothing was run",
		"",
	}, "\n")
	if out != want {
		t.Errorf("the dry run says\n%s\nwant\n%s", out, want)
	}
	if b.calls() != "" || exists(data+"/snapshots") || exists(b.path("var/lab/log/backup.ok")) || exists(b.path("var/lab/run/backup.lock")) {
		t.Errorf("the dry run did something:\n%s", b.calls())
	}
}

func TestABackupWithoutItsKeysSaysWhichIsMissing(t *testing.T) {
	b := newBackedUp(t)
	os.Remove(b.path(backupKeys + "/r2-secret-access-key"))
	out := b.refused("backup")
	if !strings.Contains(out, "no "+b.path(backupKeys)+"/r2-secret-access-key") || len(b.snapshots("house")) != 1 || len(b.snapshots("offsite")) != 0 {
		t.Errorf("without the R2 secret:\n%s", out)
	}
	b = newBackedUp(t)
	os.Remove(b.path(resticCopy))
	if out := b.refused("backup"); !strings.Contains(out, "no restic at "+b.path(resticCopy)) {
		t.Errorf("without restic:\n%s", out)
	}
	if out := b.refused("backup", "restic", "elsewhere", "snapshots"); !strings.Contains(out, "scripts/mini/backup restic house|offsite") {
		t.Errorf("a third target:\n%s", out)
	}
}

func TestOneBackupAtATime(t *testing.T) {
	b := newBackedUp(t)
	lock := b.path("var/lab/run/backup.lock")
	// A backup that is running: this test's own process holds the lock.
	write(t, lock+"/pid", strconv.Itoa(os.Getpid())+"\n", 0o600)
	if out := b.refused("backup"); !strings.Contains(out, "another backup is running") || b.told() != "" {
		t.Errorf("with a backup running:\n%s", out)
	}
	// A lock left with no pid in it, as a power cut leaves one, or with the
	// number of no process, is taken over.
	for _, pid := range []string{"", "0\n", "99999999\n", "-1\n"} {
		os.RemoveAll(lock)
		if err := os.MkdirAll(lock, 0o700); err != nil {
			t.Fatal(err)
		}
		if pid != "" {
			write(t, lock+"/pid", pid, 0o600)
		}
		if out, code := b.run("backup"); code != 0 || exists(lock) {
			t.Errorf("with a lock holding the pid %q: exit %d, lock left %v\n%s", pid, code, exists(lock), out)
		}
	}
}

// The backups and the Restore drill read restic's answers with jq, from the
// path macOS itself fills.
func TestSetupStopsWithoutJq(t *testing.T) {
	m, k := withBackups(t)
	own := filepath.Join(filepath.Dir(m.root), "macos")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/sbin", "/usr/sbin", "/bin", "/usr/bin"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "jq" {
				os.Remove(filepath.Join(own, e.Name()))
				os.Symlink(filepath.Join(dir, e.Name()), filepath.Join(own, e.Name()))
			}
		}
	}
	for i, v := range m.env {
		if strings.HasPrefix(v, "PATH=") {
			m.env[i] = "PATH=" + m.bin + ":" + own
		}
	}
	for _, args := range [][]string{{"--dry-run", "--release-key", k.signers}, {"--release-key", k.signers}} {
		if out := m.refused("setup", args...); !strings.Contains(out, "no jq") {
			t.Errorf("setup %v without jq:\n%s", args, out)
		}
	}
	if m.daemonLoaded(litestreamLabel) || m.daemonLoaded(backupLabel) {
		t.Error("a backup was started on a machine with no jq")
	}
}

// A copy a Deploy left beside a database is a whole database: like the live
// files, the house copy leaves it out.
func TestTheHouseCopyLeavesADeploysCopyBesideTheDatabasesOut(t *testing.T) {
	b := newBackedUp(t)
	for _, name := range []string{"labd.db", "reports.db"} {
		write(t, b.path("var/lab/data/"+name+".restored"), "a whole database\n", 0o600)
	}

	out := b.ok("backup")

	for _, f := range b.carried("house", "00000001") {
		if strings.HasSuffix(f, ".restored") {
			t.Errorf("the house copy carries %s\n%s", f, out)
		}
	}
}

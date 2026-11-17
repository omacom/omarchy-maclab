# The Restore drill, 2026-11-16

Ready 3. Run on the mini as the lab account (`scripts/mini/restore-drill`), house copy and off-site (R2) copy. The first run failed for want of a backup taken after the old lab's data moved in (the drill compares with the live files); one nightly run by hand, then the drill again. Then the PBS restore of the backup guest on its Proxmox host, network off, looked inside and destroyed.

```
== 2026-11-16 15:53:31 AEST restore drill
restore-drill: the house copy: snapshot 19008f55 of lab-mini, made 7 hours ago
restore-drill: failed: the house copy holds no artifact, and the mini holds 207
restore-drill: labd.db from the house copy: intact, tables: 28, rows: 14, rows in the live database now: 486
restore-drill: reports.db from the house copy: intact, tables: 5, rows: 1, rows in the live database now: 1
restore-drill: the off-site copy: snapshot 3a72dfc3 of lab-mini, made 7 hours ago
restore-drill: labd.db from the off-site copy: intact, tables: 28, rows: 14, rows in the live database now: 486
restore-drill: reports.db from the off-site copy: intact, tables: 5, rows: 1, rows in the live database now: 1
restore-drill: The Restore drill failed: the house copy holds no artifact, and the mini holds 207.
exit 1
-- restore-drill.ok
-- /var/lab/run
cat: /var/lab/log/restore-drill.ok: No such file or directory
labd.sock
ops.sock
signal-cli.sock
== 2026-11-16 15:53:45 AEST backup
[0:00] 100.00%  4 / 4 indexes processed
[0:00] 100.00%  4 / 4 old indexes deleted
removing 2 old packs
[0:00] 100.00%  2 / 2 files deleted
done
backup: done
exit 0
== 2026-11-16 15:54:31 AEST restore drill
restore-drill: the house copy: snapshot 5c8e8900 of lab-mini, made 0 hours ago
restore-drill: labd.db from the house copy: intact, tables: 28, rows: 488, rows in the live database now: 488
restore-drill: reports.db from the house copy: intact, tables: 5, rows: 1, rows in the live database now: 1
restore-drill: the artifact 617b31217a316ac926f66f0faf7c2778abd3abed638d7075814ff9348dab9b2e from the house copy: is what its name says, and the same as the live file
restore-drill: the off-site copy: snapshot eee4cad7 of lab-mini, made 0 hours ago
restore-drill: labd.db from the off-site copy: intact, tables: 28, rows: 488, rows in the live database now: 488
restore-drill: reports.db from the off-site copy: intact, tables: 5, rows: 1, rows in the live database now: 1
restore-drill: done
exit 0
-- restore-drill.ok
2026-11-16T05:54:37Z
-- /var/lab/run
labd.sock
ops.sock
signal-cli.sock
```

## The PBS restore of the backup guest

```
== 2026-11-16 15:54:34 AEST again, restic by its full path: CT 101 restored to CT 102, network off
backup: pbs-local-pve1:backup/ct/101/2026-11-15T21:58:02Z
started
-- restic snapshots
ID        Time                 Host        Tags        Paths          Size
---------------------------------------------------------------------------------
6e111b9b  2026-11-15 21:45:34  lab-mini                /var/lab/data  320.000 KiB
19008f55  2026-11-15 21:57:33  lab-mini                /var/lab/data  336.000 KiB
---------------------------------------------------------------------------------
Timestamps shown in local time
2 snapshots
-- restic check
check all packs
check snapshots, trees and blobs
[0:00] 100.00%  2 / 2 snapshots
no errors were found
-- Litestream's files: 15
  Logical volume "vm-102-disk-0" successfully removed.
destroyed CT 102
```

Result: pass

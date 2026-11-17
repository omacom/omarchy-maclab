#!/usr/bin/env python3
"""Production-function regressions using text fixtures and stubbed effects only."""
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('lab-only-autounlock')


class AutoUnlockTests(unittest.TestCase):
    def scenario(self, action='disable', current=None, images=None, staged='none',
                 active=(1, 5, 7), keep=(1, 2), stop_confirm=False,
                 rebuild_update=True, passphrase_slot=1, menu='ok'):
        functions = SCRIPT.read_text().split('# ---- main ', 1)
        self.assertEqual(len(functions), 2)
        self.assertIsNotNone(shutil.which('script'), 'util-linux script is needed for an isolated fixture tty')
        images = images or {}
        with tempfile.TemporaryDirectory(prefix='maclab-autounlock-test-') as tmp:
            root = Path(tmp)
            (root / 'scratch').mkdir()
            for label in ('A', 'B', 'DEAD', 'OWNER'):
                (root / ('payload-' + label)).write_text('fixture-not-a-disk-key-' + label)
            if current:
                (root / 'current.fixture').write_text('fixture-not-a-disk-key-' + current)
                (root / 'dropin.fixture').write_text('FILES+=(' + str(root / 'current.fixture') + ')\n')
            (root / 'effects').write_text('')
            init = '\n'.join([
                'FIX=' + shlex.quote(tmp), 'ACTION=' + shlex.quote(action),
                'KEY=$FIX/current.fixture', 'DROPIN=$FIX/dropin.fixture', 'CMDFILE=$FIX/command.fixture',
                'DEVICE=fixture-not-a-device', 'TOKEN=fixture-token', 'VOLUME=fixture',
                'STATE=$FIX/state.fixture', 'LIMINE_CONF=fixture-menu', 'KEEP=(' + ' '.join(map(str, keep)) + ')',
                'ACTIVE=(' + ' '.join(map(str, active)) + ')',
                'STAGED=' + shlex.quote(staged), 'IMAGE_STATE=present',
                'STOP_CONFIRM=' + str(int(stop_confirm)), 'REBUILD_UPDATE=' + str(int(rebuild_update)),
                'PASSPHRASE_SLOT=' + str(passphrase_slot), 'CMD_PRESENT=' + str(int(bool(current))),
                'MENU=' + shlex.quote(menu),
                'REBUILD=fixture-rebuild', 'UKI_NAME=fixture', 'ESP=$FIX',
                'declare -A CONTENT=()', 'IMAGES=()',
                *['IMAGES+=(' + shlex.quote(name) + '); CONTENT[' + shlex.quote(name) + ']=' + shlex.quote(label)
                  for name, label in images.items()],
            ])
            controls = r'''
cleanup() { :; } # no real shred/remove trap: Python owns ordinary text fixtures
note() { printf '%s\n' "$*" >>"$FIX/effects"; }
forbidden() { printf 'FORBIDDEN: %s\n' "$*" >&2; exit 99; }
probe_file() {
 PROBED=
 [[ -f $1 ]] || return 0
 local label slot
 label=$(cat "$1")
 case $label in
  fixture-not-a-disk-key-A) slot=5;;
  fixture-not-a-disk-key-B) slot=7;;
  fixture-not-a-disk-key-OWNER) slot=1;;
  fixture-not-a-disk-key-DEAD) return 0;;
  *) forbidden unexpected-fixture;;
 esac
 [[ " ${ACTIVE[*]} " != *" $slot "* ]] || PROBED=$slot
 return 0
}
keyslots() { printf '%s\n' "${ACTIVE[@]}"; }
pbkdf2_slots() { PBKDF2_SLOTS=(); } # live fixtures may be adopted argon2id slots
staged_ukis() { [[ $STAGED == none ]] || printf '%s\n' staged.efi; return 0; }
ukis() { local f; for f in "${IMAGES[@]}"; do [[ $f != *.efi ]] || printf '%s\n' "$f"; done; return 0; }
initramfs_images() { local f; for f in "${IMAGES[@]}"; do [[ $f != *.img ]] || printf '%s\n' "$f"; done; return 0; }
lsinitcpio() {
 local label=${CONTENT[$1]:-UNKNOWN}
 if [[ $1 == staged.efi ]]; then
  [[ $STAGED != unreadable ]] || return 1
  [[ $STAGED != key ]] || { printf '%s\n' etc/cryptsetup-keys.d/fixture.key; return 0; }
  printf '%s\n' etc/other; return 0
 fi
 [[ $IMAGE_STATE != clear ]] || { printf '%s\n' etc/other; return 0; }
 [[ $label != UNKNOWN ]] || return 1
 printf '%s\n' etc/cryptsetup-keys.d/fixture.key
}
make_tmpd() { TMPD=$FIX/scratch; }
extract_key() {
 local label=${CONTENT[$1]:-UNKNOWN}
 [[ $label != UNKNOWN && $label != OPAQUE ]] || return 1
 printf '%s\n' "$FIX/payload-$label"
}
uki_token() { [[ $IMAGE_STATE == clear ]] && echo no || echo yes; }
limine_entries() { (( ${#IMAGES[@]} )) || return 0; [[ $IMAGE_STATE == clear ]] && printf 'fixture\tno\n' || printf 'fixture\tyes\n'; }
menu_report() {
 (( ${#IMAGES[@]} )) || return 0
 case $MENU in
  ok) printf 'E\tmenu\t1\tfixture\tok\t-\n' ;;
  default-asks) printf 'E\tmenu\t1\tleftover\task\tfixture.efi has no key file\nE\tmenu\t0\tfixture\tlater\t-\n' ;;
 esac
}
cmdfile_has_token() { (( CMD_PRESENT )); }
crypttab_entry() { :; }
other_luks_keys() { :; }
foreign_key_configs() { :; }
check_cmdfile() { :; }
need_rebuild_tools() { :; }
need_tty() { :; }
readable_warnings() { :; }
confirm() { (( !STOP_CONFIRM )) || { echo 'fixture stops before changes'; exit 77; }; note confirm; }
edit_cmdline() { note edit; [[ $1 == add ]] && CMD_PRESENT=1 || CMD_PRESENT=0; }
rebuild() {
 note rebuild
 if (( REBUILD_UPDATE )); then
  if [[ $ACTION == disable ]]; then IMAGE_STATE=clear
  else for f in "${IMAGES[@]}"; do CONTENT[$f]=A; done
  fi
 fi
 return 0
}
cryptsetup() {
 if [[ $1 == open ]]; then printf 'Key slot %s unlocked.\n' "$PASSPHRASE_SLOT"; return 0; fi
 [[ $1 == luksRemoveKey && $2 == fixture-not-a-device ]] || forbidden cryptsetup-arguments
 probe_file "$3"
 [[ -n $PROBED ]] || forbidden removing-nonlive-fixture
 note "remove $PROBED"
 local s next=()
 for s in "${ACTIVE[@]}"; do [[ $s == "$PROBED" ]] || next+=("$s"); done
 ACTIVE=("${next[@]}")
}
rm() {
 [[ $1 == -f ]] || forbidden rm-options
 shift
 local f
 for f in "$@"; do [[ $f == "$FIX/"* ]] || forbidden rm-outside-fixture; done
 note rm
 command rm -f -- "$@"
}
install() { forbidden install; }
mv() { forbidden mv; }
chmod() { forbidden chmod; }
chown() { forbidden chown; }
cp() { forbidden cp; }
shred() { forbidden shred; }
"$ACTION"
'''
            fixture = root / 'functions-test.sh'
            fixture.write_text(functions[0] + '\n' + init + '\n' + controls)
            # A private PTY lets the production </dev/tty redirection reach only
            # our cryptsetup shell function, never any crypto executable/device.
            result = subprocess.run(
                ['script', '--quiet', '--return', '--command',
                 'bash --noprofile --norc ' + shlex.quote(str(fixture)), '/dev/null'],
                input='', text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                env={'PATH': os.environ.get('PATH', os.defpath), 'LC_ALL': 'C'}, timeout=8,
            )
            output = result.stdout + result.stderr
            self.assertNotIn('FORBIDDEN:', output)
            return result.returncode, output, (root / 'effects').read_text().splitlines()

    def assert_refused_without_effects(self, **kwargs):
        rc, output, effects = self.scenario(**kwargs)
        self.assertEqual(rc, 1, output)
        self.assertEqual(effects, [], output)
        self.assertNotIn('Already disabled:', output)
        return output

    def test_already_disabled_without_staged_images(self):
        rc, output, effects = self.scenario()
        self.assertEqual(rc, 0, output)
        self.assertIn('Already disabled:', output)
        self.assertEqual(effects, [])

    def test_staged_image_known_to_have_no_key_remains_allowed(self):
        rc, output, effects = self.scenario(staged='no-key')
        self.assertEqual(rc, 0, output)
        self.assertIn('Already disabled:', output)
        self.assertEqual(effects, [])

    def test_known_staged_key_refuses_before_mutations(self):
        self.assertIn('holds the key', self.assert_refused_without_effects(staged='key'))

    def test_unreadable_staged_image_refuses_before_mutations(self):
        self.assertIn("couldn't be read", self.assert_refused_without_effects(staged='unreadable'))

    def test_unreadable_normal_images_refuse_even_with_current_live_key(self):
        for image in ('normal.efi', 'normal.img'):
            with self.subTest(image=image):
                self.assertIn("couldn't be read", self.assert_refused_without_effects(current='A', images={image: 'UNKNOWN'}))

    def test_unextractable_normal_key_refuses_without_pbkdf2_heuristic(self):
        self.assertIn("couldn't copy out", self.assert_refused_without_effects(current='A', images={'normal.efi': 'OPAQUE'}))

    def test_unknown_image_refuses_without_current_file_or_pbkdf2_slots(self):
        for label in ('UNKNOWN', 'OPAQUE'):
            with self.subTest(label=label):
                self.assert_refused_without_effects(images={'normal.efi': label}, active=(1, 7))

    def test_enable_current_key_in_each_image_is_already_enabled(self):
        rc, output, effects = self.scenario(action='enable', current='A', images={'normal.efi': 'A', 'normal.img': 'A'})
        self.assertEqual(rc, 0, output)
        self.assertIn('Already enabled:', output)
        self.assertEqual(effects, [])

    def test_enable_dead_key_at_same_filename_cannot_short_circuit(self):
        for image in ('normal.efi', 'normal.img'):
            for label in ('DEAD', 'B'):
                with self.subTest(image=image, label=label):
                    rc, output, effects = self.scenario(action='enable', current='A', images={image: label}, stop_confirm=True)
                    self.assertEqual(rc, 77, output)
                    self.assertNotIn('Already enabled:', output)
                    self.assertEqual(effects, [])

    def test_enable_refuses_a_default_entry_that_asks_before_changes(self):
        rc, output, effects = self.scenario(action='enable', current='A', images={'normal.efi': 'DEAD'}, menu='default-asks')
        self.assertEqual(rc, 1, output)
        self.assertEqual(effects, [], output)
        self.assertIn('the default boot entry "leftover" would ask for the passphrase', output)
        self.assertIn('Nothing changed', output)
        self.assertNotIn('no passphrase prompt', output)

    def test_enable_post_rebuild_rejects_stale_key_contents(self):
        rc, output, effects = self.scenario(action='enable', current='A', images={'normal.efi': 'DEAD'}, rebuild_update=False)
        self.assertEqual(rc, 1, output)
        self.assertIn('rebuild', effects)
        self.assertNotIn('Enabled: key slot', output)

    def test_disable_checks_and_revokes_different_retained_live_key(self):
        rc, output, effects = self.scenario(current='A', images={'normal.efi': 'B'})
        self.assertEqual(rc, 0, output)
        self.assertIn('Disabled.', output)
        self.assertEqual([x for x in effects if x.startswith('remove ')], ['remove 5', 'remove 7'])
        self.assertLess(effects.index('remove 7'), effects.index('rm'))

    def test_disable_deduplicates_current_key_also_in_images(self):
        rc, output, effects = self.scenario(current='A', images={'normal.efi': 'A', 'normal.img': 'A'})
        self.assertEqual(rc, 0, output)
        self.assertEqual([x for x in effects if x.startswith('remove ')], ['remove 5'])

    def test_disable_refuses_owner_key_recovered_from_image(self):
        self.assertIn("owner's or recovery slot", self.assert_refused_without_effects(current='A', images={'normal.efi': 'OWNER'}))

    def test_disable_refuses_current_owner_key(self):
        self.assertIn("owner's or recovery slot", self.assert_refused_without_effects(current='OWNER'))

    def test_disable_refuses_removal_of_last_slot(self):
        self.assertIn('only key slot', self.assert_refused_without_effects(current='A', active=(5,), images={'normal.efi': 'A'}))

    def test_disable_refuses_removal_of_all_remaining_slots(self):
        self.assert_refused_without_effects(current='A', active=(5, 7), images={'normal.efi': 'B'})

    def test_disable_passphrase_must_open_a_slot_not_being_removed(self):
        rc, output, effects = self.scenario(current='A', images={'normal.efi': 'A'}, passphrase_slot=5)
        self.assertEqual(rc, 1, output)
        self.assertIn("couldn't confirm the passphrase", output)
        self.assertEqual(effects, ['confirm'])


AIR_UUID = 'c8761528-040e-4dad-992e-d8f1800d46e0'
# The Omarchy 4.5 rc image in the boot that encrypted its root in place: the initramfs opened
# the volume itself, so this boot's command line has no rd.luks.*; omarchy-mac-encrypt wrote
# /etc/crypttab and the next boot's rd.luks.name= in GRUB_CMDLINE_LINUX.
FIRST_BOOT_CMDLINE = ('root=UUID=4f4d5801-524f-4f54-8000-000000000001 rw rootflags=subvol=@,x-systemd.device-timeout=0 '
                      'zswap.enabled=0 rootfstype=btrfs user4k=on quiet splash loglevel=0')
FIRST_BOOT_GRUB = ('GRUB_DISTRIBUTOR="Omarchy"\n'
                   'GRUB_CMDLINE_LINUX="zswap.enabled=0 rootfstype=btrfs rootflags=x-systemd.device-timeout=0 user4k=on '
                   'rd.luks.name=' + AIR_UUID + '=root"\n'
                   'GRUB_CMDLINE_LINUX_DEFAULT="quiet splash loglevel=0"\n')
FIRST_BOOT_CRYPTTAB = 'root UUID=' + AIR_UUID + ' none luks\n'


class VolumeTests(unittest.TestCase):
    """find_volume and crypttab_entry against text fixtures of the two Omarchy Mac layouts."""

    def run_fn(self, call, cmdline=FIRST_BOOT_CMDLINE, grub=FIRST_BOOT_GRUB, crypttab=FIRST_BOOT_CRYPTTAB,
               initramfs_crypttab=None, mapping='root', volume='root', uuid=AIR_UUID):
        functions = SCRIPT.read_text().split('# ---- main ', 1)[0]
        with tempfile.TemporaryDirectory(prefix='maclab-autounlock-volume-') as tmp:
            root = Path(tmp)
            (root / 'cmdline').write_text(cmdline + '\n')
            (root / 'grub').write_text(grub)
            if crypttab is not None:
                (root / 'crypttab').write_text(crypttab)
            if initramfs_crypttab is not None:
                (root / 'crypttab.initramfs').write_text(initramfs_crypttab)
            init = '\n'.join([
                'FIX=' + shlex.quote(tmp), 'PROC_CMDLINE=$FIX/cmdline', 'ETC_CRYPTTAB=$FIX/crypttab',
                'CRYPTTAB=$FIX/crypttab.initramfs', 'CMDFILE=$FIX/grub', "CMDLINE_RE='^GRUB_CMDLINE_LINUX='",
                'VOLUME=' + shlex.quote(volume), 'UUID=' + shlex.quote(uuid),
                'root_mapping() { printf "%s\\n" ' + shlex.quote(mapping) + '; }',
                'findmnt() { echo FORBIDDEN: findmnt >&2; exit 99; }',
                'cryptsetup() { echo FORBIDDEN: cryptsetup >&2; exit 99; }',
            ])
            fixture = root / 'volume-test.sh'
            fixture.write_text(functions + '\n' + init + '\n' + call + '\n')
            result = subprocess.run(['bash', '--noprofile', '--norc', str(fixture)], stdin=subprocess.DEVNULL,
                                    text=True, capture_output=True,
                                    env={'PATH': os.environ.get('PATH', os.defpath), 'LC_ALL': 'C'}, timeout=8)
            output = result.stdout + result.stderr
            self.assertNotIn('FORBIDDEN:', output)
            return result.returncode, output

    def volume(self, **kwargs):
        return self.run_fn('find_volume; printf "%s %s %s\\n" "$UUID" "$VOLUME" "$VOLUME_FROM"', **kwargs)

    def refused(self, **kwargs):
        rc, output = self.volume(**kwargs)
        self.assertEqual(rc, 1, output)
        return output

    def test_first_boot_volume_comes_from_crypttab_and_next_cmdline(self):
        rc, output = self.volume()
        self.assertEqual(rc, 0, output)
        self.assertEqual(output.strip(), AIR_UUID + ' root crypttab')

    def test_crypttab_by_uuid_path_and_matching_initramfs_crypttab(self):
        rc, output = self.volume(crypttab='root /dev/disk/by-uuid/' + AIR_UUID.upper() + ' none luks\n',
                                 initramfs_crypttab='# comment\nroot UUID=' + AIR_UUID + ' none\n')
        self.assertEqual(rc, 0, output)
        self.assertTrue(output.strip().endswith(' root crypttab'), output)

    def test_running_cmdline_rd_luks_name_still_wins(self):
        rc, output = self.volume(cmdline=FIRST_BOOT_CMDLINE + ' rd.luks.name=' + AIR_UUID + '=root',
                                 grub='GRUB_CMDLINE_LINUX=""\n', crypttab=None)
        self.assertEqual(rc, 0, output)
        self.assertEqual(output.strip(), AIR_UUID + ' root cmdline')

    def test_running_cmdline_rd_luks_uuid_still_wins(self):
        rc, output = self.volume(cmdline=FIRST_BOOT_CMDLINE + ' rd.luks.uuid=luks-' + AIR_UUID, crypttab=None)
        self.assertEqual(rc, 0, output)
        self.assertEqual(output.strip(), AIR_UUID + ' luks-' + AIR_UUID + ' cmdline')

    def test_two_rd_luks_on_running_cmdline_refuse(self):
        self.assertIn('found 2', self.refused(cmdline=FIRST_BOOT_CMDLINE + ' rd.luks.uuid=' + AIR_UUID +
                                              ' rd.luks.name=' + AIR_UUID + '=root'))

    def test_next_cmdline_without_rd_luks_refuses(self):
        self.assertIn('rd.luks.key there would do nothing',
                      self.refused(grub='GRUB_CMDLINE_LINUX="quiet"\nGRUB_CMDLINE_LINUX_DEFAULT="rd.luks.name=' + AIR_UUID + '=root"\n'))

    def test_next_cmdline_naming_another_volume_refuses(self):
        other = '11111111-2222-3333-4444-555555555555'
        for grub in ('GRUB_CMDLINE_LINUX="rd.luks.name=' + other + '=root"\n',
                     'GRUB_CMDLINE_LINUX="rd.luks.name=' + AIR_UUID + '=cryptroot"\n',
                     'GRUB_CMDLINE_LINUX="rd.luks.uuid=' + AIR_UUID + '"\n'):
            with self.subTest(grub=grub):
                self.assertIn('not guessing which is right', self.refused(grub=grub))

    def test_next_cmdline_with_two_rd_luks_refuses(self):
        self.assertIn('expected exactly one', self.refused(
            grub='GRUB_CMDLINE_LINUX="rd.luks.name=' + AIR_UUID + '=root rd.luks.uuid=' + AIR_UUID + '"\n'))

    def test_root_not_on_a_mapping_refuses(self):
        self.assertIn("isn't on a dm-crypt volume", self.refused(mapping=''))

    def test_no_crypttab_entry_for_root_refuses(self):
        for crypttab in (None, 'home UUID=' + AIR_UUID + ' none luks\n', '#root UUID=' + AIR_UUID + ' none\n'):
            with self.subTest(crypttab=crypttab):
                self.assertIn('no crypttab entry for root', self.refused(crypttab=crypttab))

    def test_crypttab_device_not_by_uuid_refuses(self):
        self.assertIn('reads only UUID=', self.refused(crypttab='root PARTUUID=a6dcab17-cb0e-4979-900b-6d64ba44cbb9 none luks\n'))

    def test_crypttabs_disagreeing_on_uuid_refuse(self):
        self.assertIn('two LUKS UUIDs', self.refused(
            initramfs_crypttab='root UUID=11111111-2222-3333-4444-555555555555 none\n'))

    def entry(self, **kwargs):
        rc, output = self.run_fn('crypttab_entry', **kwargs)
        self.assertEqual(rc, 0, output)
        return output.strip()

    def test_plain_etc_crypttab_line_does_not_reach_the_initramfs(self):
        self.assertEqual(self.entry(), '')

    def test_x_initrd_attach_line_reaches_the_initramfs(self):
        self.assertIn('x-initrd.attach', self.entry(crypttab='root UUID=' + AIR_UUID + ' none luks,x-initrd.attach\n'))

    def test_initramfs_crypttab_shadows_etc_crypttab(self):
        self.assertEqual(self.entry(crypttab='root UUID=' + AIR_UUID + ' none x-initrd.attach\n',
                                    initramfs_crypttab='swap /dev/sda2 /dev/urandom swap\n'), '')
        self.assertIn('crypttab.initramfs: root', self.entry(initramfs_crypttab='root UUID=' + AIR_UUID + ' none\n'))


MENU_TOKEN = 'rd.luks.key=' + AIR_UUID + '=/etc/cryptsetup-keys.d/root.key'
OMARCHY_CMDLINE = 'root=UUID=x rw rd.luks.name=' + AIR_UUID + '=root ' + MENU_TOKEN + ' quiet'
LEFTOVER = """/Aurora lab 7.2.11-lab0bc6e4fa-e937
    comment: M2 sched-rampup lab kernel
    protocol: efi
    path: boot():/maclab-m2/7.2.11-lab0bc6e4fa-e937/uki.efi
"""
OMARCHY = """/+Omarchy
### This OS entry is auto-generated by limine-entry-tool
comment: Omarchy
  //linux-aurora
  protocol: efi
  path: boot():/EFI/Linux/omarchy_linux-aurora.efi#5c7f06
  cmdline: """ + OMARCHY_CMDLINE + """
     //Snapshots
     comment: 1 snapshot
     ///2 │ 2026-10-13 23:05:11
     ////linux-aurora
     protocol: efi
     path: boot():/1111/limine_history/omarchy_linux-aurora.efi_sha256_2e71#cc42
     cmdline: root=UUID=x rw rootflags=subvol=/@/.snapshots/2/snapshot
"""
LAB_BLOCK = """# maclab: begin
/maclab-j7
    comment: maclab j7: 7.2.11-lab
    protocol: efi
    path: boot():/maclab/j7/uki.efi
# maclab: end
"""
# What each fixture image on the ESP holds: its key file, and its embedded cmdline's TOKEN.
MENU_IMAGES = {
    'EFI/Linux/omarchy_linux-aurora.efi': 'key=current token=yes',
    'maclab-m2/7.2.11-lab0bc6e4fa-e937/uki.efi': 'key=none token=no',
    '1111/limine_history/omarchy_linux-aurora.efi_sha256_2e71': 'key=old token=no',
    'maclab/j7/uki.efi': 'key=current token=yes',
    'EFI/tools/shell.efi': 'kind=other',
}


class MenuTests(unittest.TestCase):
    """menu_entries, menu_status and refuse_menu against limine.conf fixtures of an Omarchy Mac."""

    def run_fn(self, call, conf, key_slot='5', images=None):
        functions = SCRIPT.read_text().split('# ---- main ', 1)[0]
        with tempfile.TemporaryDirectory(prefix='maclab-autounlock-menu-') as tmp:
            root = Path(tmp)
            esp = root / 'esp'
            for rel, content in (MENU_IMAGES if images is None else images).items():
                (esp / rel).parent.mkdir(parents=True, exist_ok=True)
                (esp / rel).write_text(content + '\n')
            (esp / 'limine.conf').write_text(conf)
            init = '\n'.join([
                'FIX=' + shlex.quote(tmp), 'ESP=$FIX/esp', 'LIMINE_CONF=$ESP/limine.conf', 'UKI_NAME=omarchy',
                'TOKEN=' + shlex.quote(MENU_TOKEN), 'KEY=/etc/cryptsetup-keys.d/root.key', 'VOLUME=root',
                'KEY_SLOT=' + shlex.quote(key_slot), 'REBUILD=omarchy-mac-boot-update',
                'uki_kind() { grep -q kind=other "$1" && echo other || echo uki; }',
                'image_key() { grep -q key=none "$1" && echo no || echo yes; }',
                'image_current_key() { grep -q key=current "$1"; }',
                'uki_token() { grep -q token=yes "$1" && echo yes || echo no; }',
                'cryptsetup() { echo FORBIDDEN: cryptsetup >&2; exit 99; }',
                'objcopy() { echo FORBIDDEN: objcopy >&2; exit 99; }',
            ])
            fixture = root / 'menu-test.sh'
            fixture.write_text(functions + '\n' + init + '\n' + call + '\n')
            result = subprocess.run(['bash', '--noprofile', '--norc', str(fixture)], stdin=subprocess.DEVNULL,
                                    text=True, capture_output=True,
                                    env={'PATH': os.environ.get('PATH', os.defpath), 'LC_ALL': 'C'}, timeout=8)
            output = (result.stdout + result.stderr).replace(tmp, '<fix>')
            self.assertNotIn('FORBIDDEN:', output)
            return result.returncode, output

    def default(self, conf):
        """The default entry's name, or the D line's reason."""
        rc, output = self.run_fn('menu_entries', conf)
        self.assertEqual(rc, 0, output)
        lines = [l.split('\t') for l in output.splitlines()]
        named = [f[3] for f in lines if f[0] == 'E' and f[2] == '1']
        reasons = [f[1] for f in lines if f[0] == 'D']
        self.assertEqual(len(named) + len(reasons), 1, output)
        return (named or reasons)[0]

    def test_default_entry_counts_the_entries_the_menu_first_shows(self):
        menu = LEFTOVER + OMARCHY
        for dflt, want in (('', 'Aurora lab 7.2.11-lab0bc6e4fa-e937'),
                           ('default_entry: 1\n', 'Aurora lab 7.2.11-lab0bc6e4fa-e937'),
                           ('default_entry: 3\n', 'Omarchy / linux-aurora'),
                           ('DEFAULT_ENTRY=3\n', 'Omarchy / linux-aurora'),
                           ('default_entry: Omarchy/linux-aurora\n', 'Omarchy / linux-aurora'),
                           ('default_entry: Omarchy/Snapshots/2 │ 2026-10-13 23:05:11/linux-aurora\n',
                            'Omarchy / Snapshots / 2 │ 2026-10-13 23:05:11 / linux-aurora')):
            with self.subTest(dflt=dflt):
                self.assertEqual(self.default('timeout: 3\n' + dflt + menu), want)
        for dflt, why in (('default_entry: 2\n', 'is the directory "Omarchy"'),
                          ('default_entry: 4\n', 'is the directory "Snapshots"'),
                          ('default_entry: 5\n', 'names no entry (the menu first shows 4'),
                          ('default_entry: 0\n', 'names no entry'),
                          ('default_entry: Omarchy/linux\n', 'names no entry')):
            with self.subTest(dflt=dflt):
                self.assertIn(why, self.default('timeout: 3\n' + dflt + menu))

    def test_entries_inside_a_closed_directory_are_not_counted(self):
        closed = OMARCHY.replace('/+Omarchy', '/Omarchy')
        self.assertIn('names no entry', self.default('default_entry: 3\n' + closed + LEFTOVER))
        self.assertEqual(self.default('default_entry: 2\n' + closed + LEFTOVER), 'Aurora lab 7.2.11-lab0bc6e4fa-e937')
        self.assertEqual(self.default('default_entry: 4\n' + OMARCHY + LEFTOVER), 'Aurora lab 7.2.11-lab0bc6e4fa-e937')

    def test_duplicate_names_take_a_hash_count(self):
        two = '/A\n  protocol: efi\n  path: boot():/x.efi\n/A\n  protocol: efi\n  path: boot():/y.efi\n'
        rc, output = self.run_fn('menu_entries', 'default_entry: A#2\n' + two)
        self.assertIn('boot():/y.efi', [l for l in output.splitlines() if l.split('\t')[2:3] == ['1']][0])

    def test_status_says_the_leftover_default_asks_for_the_passphrase(self):
        rc, output = self.run_fn('menu_status', 'default_entry: 1\n' + LEFTOVER + OMARCHY + LAB_BLOCK)
        self.assertEqual(rc, 0, output)
        lines = output.splitlines()
        self.assertTrue(lines[0].startswith('boot entry "Aurora lab 7.2.11-lab0bc6e4fa-e937" (THE DEFAULT): ASKS FOR THE PASSPHRASE: '
                                            '<fix>/esp/maclab-m2/7.2.11-lab0bc6e4fa-e937/uki.efi doesn\'t hold the key file'), output)
        self.assertIn('Remove the entry from <fix>/esp/limine.conf', lines[0])
        self.assertIn('boot entry "Omarchy / linux-aurora": has the key and ' + MENU_TOKEN + ', boots without the passphrase', output)
        self.assertIn('boot entry "Omarchy / Snapshots / 2 │ 2026-10-13 23:05:11 / linux-aurora" (a snapshot, booted by hand): asks for the passphrase', output)
        self.assertIn('boot entry "maclab-j7" (lab-agent\'s): has the key', output)

    def test_enable_refuses_the_leftover_default_before_anything_changes(self):
        for slot in ('5', ''):  # enabled already, or the first enable with no key slot yet
            with self.subTest(key_slot=slot):
                rc, output = self.run_fn('refuse_menu; echo PASSED', 'default_entry: 1\n' + LEFTOVER + OMARCHY, key_slot=slot)
                self.assertEqual(rc, 1, output)
                self.assertNotIn('PASSED', output)
                self.assertIn('the default boot entry "Aurora lab 7.2.11-lab0bc6e4fa-e937" would ask for the passphrase', output)
                self.assertIn('remove that entry from <fix>/esp/limine.conf', output)
                self.assertIn('Nothing changed', output)

    def test_enable_refuses_a_menu_entry_it_cant_rebuild_even_when_not_the_default(self):
        rc, output = self.run_fn('refuse_menu; echo PASSED', 'default_entry: 2\n' + OMARCHY + LEFTOVER)
        self.assertEqual(rc, 1, output)
        self.assertIn('boot entry "Aurora lab 7.2.11-lab0bc6e4fa-e937" would ask for the passphrase', output)
        self.assertNotIn('default boot entry', output)

    def test_enable_passes_the_menu_the_rebuild_covers(self):
        conf = 'default_entry: 2\n' + OMARCHY + LAB_BLOCK + '/EFI shell\n  protocol: efi\n  path: boot():/EFI/tools/shell.efi\n'
        rc, output = self.run_fn('refuse_menu; echo PASSED', conf, key_slot='')
        self.assertEqual((rc, output.strip()), (0, 'PASSED'))
        # before the rebuild the Omarchy UKI may still lack the key: the check after it decides
        images = dict(MENU_IMAGES, **{'EFI/Linux/omarchy_linux-aurora.efi': 'key=none token=no'})
        rc, output = self.run_fn('refuse_menu; echo PASSED', conf, images=images)
        self.assertEqual((rc, output.strip()), (0, 'PASSED'))
        rc, output = self.run_fn('menu_refusals "$(menu_report post)"; printf "%s|" "$MENU_DEFAULT" "${REFUSALS[@]}"', conf, images=images)
        self.assertTrue(output.startswith('Omarchy / linux-aurora|the default boot entry "Omarchy / linux-aurora" would ask'), output)
        rc, output = self.run_fn('menu_refusals "$(menu_report post)"; printf "%s|" "$MENU_DEFAULT" "${REFUSALS[@]}"', conf)
        self.assertEqual(output, 'Omarchy / linux-aurora|')

    def test_the_cmdline_that_boots_decides(self):
        own = OMARCHY.replace(' ' + MENU_TOKEN, '')
        rc, output = self.run_fn('menu_refusals "$(menu_report post)"; printf "%s\\n" "${REFUSALS[@]}"', 'default_entry: 2\n' + own)
        self.assertIn('its cmdline in <fix>/esp/limine.conf has no ' + MENU_TOKEN, output)
        bare = '/Omarchy\n  protocol: efi\n  path: boot():/EFI/Linux/omarchy_linux-aurora.efi\n'
        for content, ok in (('key=current token=yes', True), ('key=current token=no', False)):
            with self.subTest(content=content):
                images = dict(MENU_IMAGES, **{'EFI/Linux/omarchy_linux-aurora.efi': content})
                rc, output = self.run_fn('menu_refusals "$(menu_report post)"; printf "%s\\n" "${REFUSALS[@]}"', bare, images=images)
                self.assertEqual(output.strip() == '', ok, output)

    def test_a_default_that_boots_no_linux_or_names_nothing_refuses(self):
        shell = '/EFI shell\n  protocol: efi\n  path: boot():/EFI/tools/shell.efi\n'
        for conf, why in (('default_entry: 1\n' + shell + OMARCHY, 'the default boot entry "EFI shell" boots no Linux'),
                          ('default_entry: 9\n' + OMARCHY, 'default_entry: 9 names no entry'),
                          ('default_entry: 1\n' + OMARCHY, 'is the directory "Omarchy", so Limine stops at its menu'),
                          ('timeout: 3\n', 'no entry in <fix>/esp/limine.conf boots anything')):
            with self.subTest(conf=conf[:20]):
                rc, output = self.run_fn('refuse_menu', conf)
                self.assertEqual(rc, 1, output)
                self.assertIn(why, output)

    def test_remember_last_entry_is_a_warning(self):
        rc, output = self.run_fn('menu_status', 'remember_last_entry: yes\ndefault_entry: 2\n' + OMARCHY)
        self.assertIn('WARNING: remember_last_entry: yes', output)


if __name__ == '__main__':
    unittest.main()

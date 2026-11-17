package reports

// Forgetting a person, in reports.db: the handle and account id come off
// their reports and off the reports the lab published at their asking, and
// their Mac sign-ins go. The reports themselves stay, Published, with
// Verified or Community and the machine id; their digests stay too, so a
// deleted report cannot come back. Where the person's account id stood, a
// negative number drawn for this forgetting stands: no account has one, it
// says nothing of whose it was, and the person's reports still count as
// one person's where machines are counted.

import (
	"crypto/rand"
	"encoding/binary"
)

// Forget takes the person with this account id and these handles off
// reports.db, the handle of today first, then those they had before. A
// report kept from before the lab knew account ids names its person by
// handle alone, and goes by any of them.
func (s *Store) Forget(accountID int64, handles ...string) error {
	if len(handles) == 0 {
		handles = []string{""}
	}
	var b [8]byte
	rand.Read(b[:])
	stands := -1 - int64(binary.BigEndian.Uint64(b[:])>>2)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, handle := range handles {
		for _, q := range []string{
			`UPDATE reports SET handle = NULL, account_id = ?3 WHERE account_id = ?1 OR (account_id IS NULL AND ?2 != '' AND handle = ?2 COLLATE NOCASE)`,
			`UPDATE reports SET asked_handle = NULL, asked_account_id = ?3 WHERE asked_account_id = ?1 OR (asked_account_id IS NULL AND ?2 != '' AND asked_handle = ?2 COLLATE NOCASE)`,
		} {
			if _, err := tx.Exec(q, accountID, handle, stands); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM mac_sign_ins WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// NameNotKept reports whether the report's Mac was signed in as a person
// who has since been Forgotten.
func (r *Report) NameNotKept() bool { return r.signedInForgotten }

// AskedNameNotKept reports whether the report was published at the asking
// of a person who has since been Forgotten.
func (r *Report) AskedNameNotKept() bool { return r.askedForgotten }

package server

// Who a person is to the lab, and who may change it.
//
// A person's kind is read from four things: the configuration (Admin), two
// stored marks (Maintainer, the M team's) and the fleet (lending a Mac makes
// an External volunteer of someone with no mark). Blocked is apart from
// kind: it shuts the account out, takes its marks and its leave to lend, and
// while it lasts none of them is given back.
//
// The rules of who may change whom live here, one function a change. Each
// writes its line of the People record and tells the owner's group.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// blockedSentence is all a Blocked person is told, wherever they meet the
// lab: no reason and no name.
const blockedSentence = reports.BlockedSentence

// Role is the person's kind as the interface's table has it. A Blocked
// person signs in nowhere, so it is never asked of one in earnest.
func (p Person) Role() web.Kind {
	switch {
	case p.Admin:
		return web.Admin
	case p.Maintainer:
		return web.Maintainer
	case p.MTeam:
		return web.MTeam
	case p.Volunteer:
		return web.External
	}
	return web.NoRole
}

// OnMTeam reports whether the person is on the Omarchy M team: by the mark,
// or as a Maintainer or an Admin, who always are. It decides a Lease and a
// Verified run.
func (p Person) OnMTeam() bool { return !p.Blocked && (p.Admin || p.Maintainer || p.MTeam) }

// MayLend reports whether the person has leave to lend a Mac: the M team
// has it, and so does anyone with an Invite or an approved Request to lend.
func (p Person) MayLend() bool { return !p.Blocked && (p.OnMTeam() || p.Lend) }

// personColumns are the stored half of a Person, for a query whose people
// table goes by p; personFields is where they scan to.
const personColumns = `p.account_id, p.handle, p.maintainer, p.m_team, p.leave_to_lend, p.blocked_at IS NOT NULL`

func personFields(p *Person) []any {
	return []any{&p.AccountID, &p.Handle, &p.Maintainer, &p.MTeam, &p.Lend, &p.Blocked}
}

// fillIn sets the half of p that is not stored with the person: Admin comes
// from the configuration and Volunteer from the fleet.
func (s *Server) fillIn(p *Person) {
	p.Admin = slices.Contains(s.cfg.Admins, p.AccountID)
	p.Volunteer = len(s.macsLentBy(p.AccountID)) > 0
}

// person is the Person the lab keeps under this account id, with their
// roles at this moment; sql.ErrNoRows when the lab does not know them.
func (s *Server) person(accountID int64) (Person, error) {
	k, err := s.store.knownPerson(accountID)
	if err != nil {
		return Person{}, err
	}
	s.fillIn(&k.Person)
	return k.Person, nil
}

// macsLentBy names the enrolled Macs the person lends, in order.
func (s *Server) macsLentBy(accountID int64) []string {
	var names []string
	for _, v := range s.allDevs() {
		v.mu.Lock()
		if v.d.VolunteerID == accountID && accountID != 0 && !v.forgotten && v.d.State != api.StateLeft {
			names = append(names, v.d.Name)
		}
		v.mu.Unlock()
	}
	return names
}

// knownPerson is a Person with what else the lab keeps of them.
type knownPerson struct {
	Person
	Added time.Time
	// LastSeen is zero for a handle marked or invited that has not signed
	// in yet.
	LastSeen      time.Time
	BlockedAt     time.Time
	BlockedBy     int64
	BlockedReason string
}

const knownColumns = personColumns + `, p.added, COALESCE(p.last_seen, 0), COALESCE(p.blocked_at, 0), COALESCE(p.blocked_by, 0), p.blocked_reason`

func scanKnown(scan func(...any) error) (k knownPerson, err error) {
	var added, seen, blocked int64
	if err = scan(append(personFields(&k.Person), &added, &seen, &blocked, &k.BlockedBy, &k.BlockedReason)...); err != nil {
		return k, err
	}
	k.Added = time.UnixMicro(added)
	if seen != 0 {
		k.LastSeen = time.UnixMicro(seen)
	}
	if blocked != 0 {
		k.BlockedAt = time.UnixMicro(blocked)
	}
	return k, nil
}

func (st *store) knownPerson(accountID int64) (knownPerson, error) {
	return scanKnown(st.db.QueryRow(`SELECT `+knownColumns+` FROM people p WHERE p.account_id = ?`, accountID).Scan)
}

// knownPeople is everyone the lab knows, by handle.
func (st *store) knownPeople() ([]knownPerson, error) {
	rows, err := st.db.Query(`SELECT ` + knownColumns + ` FROM people p ORDER BY lower(p.handle), p.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []knownPerson
	for rows.Next() {
		k, err := scanKnown(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// know keeps the account GitHub names with this handle without a sign-in:
// a new Person who has not been seen, or the one the lab has, under the
// handle it has now.
func (st *store) know(id int64, handle string, now time.Time) error {
	_, err := st.db.Exec(`INSERT INTO people (account_id, handle, added) VALUES (?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET handle = excluded.handle`, id, handle, now.UnixMicro())
	return err
}

// setMark sets one stored mark of a person the lab knows.
func (st *store) setMark(accountID int64, column string, on bool) error {
	if !slices.Contains([]string{"maintainer", "m_team", "leave_to_lend"}, column) {
		return fmt.Errorf("people has no mark %q", column)
	}
	res, err := st.db.Exec(`UPDATE people SET `+column+` = ? WHERE account_id = ? AND blocked_at IS NULL`, on, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// block shuts the account out: its marks and its leave to lend go, and so
// does every browser session, in one step. Its Lab tokens stay as rows that
// open nothing, without the address of their last use, so that lab is told
// the one sentence; unblock takes them away.
func (st *store) block(accountID, by int64, reason string, now time.Time) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE people SET blocked_at = ?, blocked_by = ?, blocked_reason = ?, maintainer = 0, m_team = 0, leave_to_lend = 0
		WHERE account_id = ? AND blocked_at IS NULL`, now.UnixMicro(), by, reason, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`DELETE FROM browser_sessions WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE lab_tokens SET last_used = NULL, last_used_address = '' WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// unblock lets the account sign in again. The Lab tokens it had when it
// was Blocked go for good.
func (st *store) unblock(accountID int64) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE people SET blocked_at = NULL, blocked_by = NULL, blocked_reason = '' WHERE account_id = ? AND blocked_at IS NOT NULL`, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`DELETE FROM lab_tokens WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// errUnknownPerson is a change asked for an account the lab does not know.
var errUnknownPerson = refuse(http.StatusNotFound, "The lab knows no such person.")

// subject is the person a change is about, as the lab knows them now.
func (s *Server) subject(accountID int64) (knownPerson, error) {
	k, err := s.store.knownPerson(accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return k, errUnknownPerson
	}
	if err == nil {
		s.fillIn(&k.Person)
	}
	return k, err
}

// changed ends the person's open requests: their roles are read again on
// the next one.
func (s *Server) changed(accountID int64) {
	s.conns.end(accountConnKey(accountID))
	// Whether their Macs are Counted may have changed with the M team.
	s.publishPerf()
}

// handleTyped is what a Maintainer typed as a GitHub handle, without the @
// people put before one.
func handleTyped(raw string) (string, error) {
	handle := strings.TrimPrefix(strings.TrimSpace(raw), "@")
	if !reHandle.MatchString(handle) || strings.HasSuffix(handle, "-") || strings.Contains(handle, "--") {
		return "", refuse(http.StatusUnprocessableEntity, "%q isn't a GitHub handle.", clip(strings.TrimSpace(raw), 60))
	}
	return handle, nil
}

// pin asks GitHub which account a handle names now and keeps that account,
// so a mark or an Invite hangs on the account id from the start and waits
// for no sign-in.
func (s *Server) pin(ctx context.Context, raw string) (k knownPerson, err error) {
	handle, err := handleTyped(raw)
	if err != nil {
		return k, err
	}
	if s.cfg.GitHub == nil {
		return k, refuse(http.StatusServiceUnavailable, "This lab has no GitHub app to ask about @%s.", handle)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	who, err := s.cfg.GitHub.lookup(ctx, handle)
	switch {
	case errors.Is(err, errNoSuchAccount):
		return k, refuse(http.StatusUnprocessableEntity, "GitHub knows no @%s.", handle)
	case err != nil:
		return k, refuse(http.StatusServiceUnavailable, "GitHub didn't say who @%s is (%v). Nothing was changed: try again.", handle, err)
	}
	// A Blocked person stays as the lab has them: nothing is given to one.
	if had, err := s.subject(who.ID); err == nil && had.Blocked {
		return had, nil
	}
	if err := s.store.know(who.ID, who.Login, s.now()); err != nil {
		return k, err
	}
	return s.subject(who.ID)
}

func unblockFirst(k knownPerson) error {
	return refuse(http.StatusConflict, "@%s is Blocked. Unblock first.", k.Handle)
}

func notYet(k knownPerson) string {
	if k.LastSeen.IsZero() {
		return " (not signed in yet)"
	}
	return ""
}

// markMTeam marks the person as a member of the Omarchy M team.
func (s *Server) markMTeam(r *http.Request, k knownPerson) error {
	switch {
	case k.Blocked:
		return unblockFirst(k)
	case k.Admin:
		return refuse(http.StatusConflict, "@%s is an Admin, and an Admin is always on the M team.", k.Handle)
	case k.OnMTeam():
		return refuse(http.StatusConflict, "@%s is already on the M team.", k.Handle)
	}
	if err := s.store.setMark(k.AccountID, "m_team", true); err != nil {
		return err
	}
	s.changed(k.AccountID)
	s.agents.MTeamMarked(PersonOf(r).Handle, k.Handle)
	return s.record(r, "marked as M team"+notYet(k), &k.Person)
}

// mayUnmarkMTeam is why k's mark cannot be taken away, or nil.
func mayUnmarkMTeam(k knownPerson) error {
	switch {
	case k.Admin:
		return refuse(http.StatusConflict, "@%s is an Admin, and an Admin is always on the M team.", k.Handle)
	case k.Maintainer:
		return refuse(http.StatusConflict, "@%s is a Maintainer, and a Maintainer is always on the M team: an Admin removes the role first.", k.Handle)
	case !k.MTeam:
		return refuse(http.StatusConflict, "@%s is not on the M team.", k.Handle)
	}
	return nil
}

// unmarkMTeam takes the mark away. The person's Leases end at once. Their
// Macs stay enrolled with no new approval, so lending any they keep leave
// to lend; lending none they have what they had before the mark.
func (s *Server) unmarkMTeam(r *http.Request, k knownPerson) error {
	if err := mayUnmarkMTeam(k); err != nil {
		return err
	}
	if k.Volunteer && !k.Lend {
		if err := s.store.setMark(k.AccountID, "leave_to_lend", true); err != nil {
			return err
		}
	}
	if err := s.store.setMark(k.AccountID, "m_team", false); err != nil {
		return err
	}
	s.changed(k.AccountID)
	s.endLeasesOf(k.Person, leftTheTeam(*PersonOf(r)))
	s.agents.MTeamUnmarked(PersonOf(r).Handle, k.Handle)
	return s.record(r, "removed from the M team", &k.Person)
}

// makeMaintainer is an Admin's: the role, and with it the M team's mark.
func (s *Server) makeMaintainer(r *http.Request, k knownPerson) error {
	switch {
	case !PersonOf(r).Admin:
		return refuse(http.StatusForbidden, "Only an Admin makes a Maintainer.")
	case k.Blocked:
		return unblockFirst(k)
	case k.Admin:
		return refuse(http.StatusConflict, "@%s is an Admin: Admins are named in the server's configuration, not here.", k.Handle)
	case k.Maintainer:
		return refuse(http.StatusConflict, "@%s is already a Maintainer.", k.Handle)
	}
	if err := s.store.setMark(k.AccountID, "m_team", true); err != nil {
		return err
	}
	if err := s.setMaintainer(k.AccountID, true); err != nil {
		return err
	}
	s.agents.MaintainerAdded(PersonOf(r).Handle, k.Handle)
	return s.record(r, "made a Maintainer of", &k.Person)
}

func mayRemoveMaintainer(by *Person, k knownPerson) error {
	switch {
	case !by.Admin:
		return refuse(http.StatusForbidden, "Only an Admin removes a Maintainer.")
	case k.Admin:
		return refuse(http.StatusConflict, "@%s is an Admin: Admins are named in the server's configuration, not here.", k.Handle)
	case !k.Maintainer:
		return refuse(http.StatusConflict, "@%s is no Maintainer.", k.Handle)
	}
	return nil
}

// removeMaintainer is an Admin's too. The person stays on the M team.
func (s *Server) removeMaintainer(r *http.Request, k knownPerson) error {
	if err := mayRemoveMaintainer(PersonOf(r), k); err != nil {
		return err
	}
	if err := s.setMaintainer(k.AccountID, false); err != nil {
		return err
	}
	s.endJoinCommandsOf(k.Person, *PersonOf(r), true)
	s.agents.MaintainerRemoved(PersonOf(r).Handle, k.Handle)
	return s.record(r, "removed as Maintainer", &k.Person)
}

// invite gives the person leave to lend, from their first sign-in on.
func (s *Server) invite(r *http.Request, k knownPerson) error {
	switch {
	case k.Blocked:
		return unblockFirst(k)
	case k.MayLend():
		return refuse(http.StatusConflict, "@%s already has leave to lend.", k.Handle)
	}
	if err := s.store.setMark(k.AccountID, "leave_to_lend", true); err != nil {
		return err
	}
	s.changed(k.AccountID)
	by := PersonOf(r).Handle
	s.agents.Invited(by, k.Handle)
	s.labGroup.InviteMade(by)
	return s.record(r, "gave an Invite"+notYet(k)+" to", &k.Person)
}

// blockReason is the optional reason typed beside a Block: one line.
func blockReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if len([]rune(reason)) > 200 || strings.ContainsFunc(reason, unicode.IsControl) {
		return "", refuse(http.StatusUnprocessableEntity, "A Block's reason is one line of up to 200 characters.")
	}
	return reason, nil
}

// mayBlock is why the person signed in on r may not block k, or nil.
func mayBlock(by *Person, k knownPerson) error {
	switch {
	case k.Admin:
		return refuse(http.StatusForbidden, "An Admin cannot be Blocked.")
	case k.AccountID == by.AccountID:
		return refuse(http.StatusForbidden, "Nobody blocks themselves.")
	case k.Blocked:
		return refuse(http.StatusConflict, "@%s is already Blocked.", k.Handle)
	case k.Maintainer && !by.Admin:
		return refuse(http.StatusForbidden, "Only an Admin blocks a Maintainer.")
	}
	return nil
}

// blockEnds says what a Block of k ends now, as the button that asks twice
// states it.
func (s *Server) blockEnds(k knownPerson) string {
	var ends []string
	if n, _ := s.store.countSessions(k.AccountID, s.now()); n > 0 {
		ends = append(ends, counted(n, "browser", "browsers")+" signed out")
	}
	if tokens, _ := s.store.labTokens(k.AccountID); len(tokens) > 0 {
		n := len(tokens)
		ends = append(ends, counted(n, "Lab token", "Lab tokens")+" stopped")
	}
	if n := len(s.leasesOf(k.Person)); n > 0 {
		ends = append(ends, counted(n, "Lease", "Leases")+" ended")
	}
	if signIns, _ := s.reports.SignInsOf(k.AccountID); len(signIns) > 0 {
		ends = append(ends, counted(len(signIns), "Mac", "Macs")+" signed out")
	}
	if n := len(s.macsLentBy(k.AccountID)); n > 0 {
		ends = append(ends, counted(n, "Mac leaves", "Macs leave")+" the lab")
	}
	switch {
	case k.Maintainer:
		ends = append(ends, "no longer a Maintainer")
	case k.MTeam:
		ends = append(ends, "no longer M team")
	}
	if k.Lend {
		ends = append(ends, "leave to lend taken back")
	}
	if len(ends) == 0 {
		return "Block now: this account signs in no more"
	}
	return "Block now: " + strings.Join(ends, ", ")
}

// block shuts k out of the lab. Sign-in, every browser session, the Lab
// tokens, the Leases, the Mac sign-ins, the M team's mark, the Maintainer
// role and leave to lend end with it.
func (s *Server) block(r *http.Request, k knownPerson, reason string) error {
	by := PersonOf(r)
	if err := mayBlock(by, k); err != nil {
		return err
	}
	reason, err := blockReason(reason)
	if err != nil {
		return err
	}
	if err := s.store.block(k.AccountID, by.AccountID, reason, s.now()); err != nil {
		return err
	}
	s.changed(k.AccountID)
	s.endLendingOf(k.Person, *by)
	s.endLeasesOf(k.Person, wasBlocked(*by))
	s.endMacSignInsOf(k.AccountID)
	ended := s.endEnrollmentsOf(k.AccountID, leaving{how: leftBlocked, by: *by})
	s.agents.Blocked(by.Handle, k.Handle)
	if err := s.recordNoted(r, "blocked", &k.Person, reason); err != nil {
		return err
	}
	if ended != nil {
		return refuse(http.StatusInternalServerError, "Blocked, but a Mac they lend could not leave the lab and its secret still works (%v): end its enrollment beside it.", ended)
	}
	return nil
}

// unblock gives sign-in back, and nothing else: no role, no leave to lend,
// no Mac and no token.
func (s *Server) unblock(r *http.Request, k knownPerson) error {
	if !k.Blocked {
		return refuse(http.StatusConflict, "@%s is not Blocked.", k.Handle)
	}
	if err := s.store.unblock(k.AccountID); err != nil {
		return err
	}
	s.agents.Unblocked(PersonOf(r).Handle, k.Handle)
	return s.record(r, "unblocked", &k.Person)
}

// access is what a request by this person may do on labd's API.
func (p Person) access() access {
	switch {
	case p.Blocked:
		return blockedAccess
	case p.isMaintainer():
		return adminAccess
	case p.MTeam:
		return teamAccess
	case p.Volunteer:
		return ownAccess
	}
	return signedInAccess
}

// What else a Block ends: endMacSignInsOf is in mac_signin.go and
// endEnrollmentsOf in leaving.go.

// endLendingOf takes back what a Blocked person had on the way to lending a
// Mac: a waiting Request to lend, which is no event of its own, and every
// unused Join command they made.
func (s *Server) endLendingOf(p, by Person) {
	if _, err := s.store.settleRequest(p.AccountID, requestWithdrawn, p.AccountID, "", s.now()); err != nil {
		s.log.Printf("a Blocked person's Request to lend: %v", err)
	}
	s.endJoinCommandsOf(p, by, false)
}

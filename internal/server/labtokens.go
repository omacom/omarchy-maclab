package server

// A Lab token is a person's credential for lab, lab mcp and their agents. It
// acts as the person, with their roles at that moment, on labd's API and on
// nothing else: it opens no page, so it cannot be turned into a browser's
// session or another token. A person makes up to 10 on their own page; each
// is shown once and kept as a hash, with a name, when it was made, when it
// expires and when and from which address it was last used. Its period is
// counted from its last use, so a token in use never expires; an expired
// one stays listed until its person renews it, and then the same secret
// works again.
//
// Three cuts end what a stolen token can do, smallest first: revoking the
// token, "End all access", and Blocked. "Sign out everywhere" ends browsers
// only.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	labTokenPrefix = "lab_"
	// labTokenCap is how many Lab tokens one person has at most, expired
	// ones included.
	labTokenCap = 10
	// labTokenTouch is how often at most a token's use is written down
	// while it comes from one address.
	labTokenTouch = time.Minute
	// breakGlassPeriod is the period of a token the server's break-glass
	// command mints: the shortest.
	breakGlassPeriod = 30
	day              = 24 * time.Hour
)

// labTokenPeriods are the periods a person chooses from, in days unused; 0
// is never.
var labTokenPeriods = []int{30, 90, 365, 0}

const labTokenDefaultPeriod = 90

func newLabToken() string { return labTokenPrefix + randHex(20) }

// labToken is one Lab token as labd.db keeps it, without its hash.
type labToken struct {
	ID        int64
	AccountID int64
	Name      string
	Period    int // days unused; 0 never
	Made      time.Time
	Expires   time.Time // zero: never
	LastUsed  time.Time // zero: not used yet
	Address   string    // of the last use
	told      bool
}

func (t labToken) expired(now time.Time) bool { return !t.Expires.IsZero() && !now.Before(t.Expires) }

// expiry is when a token of period days expires if it is used now and never
// again; zero for never.
func expiry(period int, now time.Time) time.Time {
	if period == 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(period) * day)
}

// tokenUse is the person a request's Lab token belongs to, with their roles
// at this moment.
type tokenUse struct {
	Person
	token   labToken
	expired bool
}

type tokenUseKey struct{}

func tokenUseOf(r *http.Request) *tokenUse {
	u, _ := r.Context().Value(tokenUseKey{}).(*tokenUse)
	return u
}

// connKeys name a request's place in Server.conns, so that each cut ends
// the right open requests: every one of the person's, their browsers', one
// browser session's, or one token's.
func accountConnKey(accountID int64) string { return "account:" + strconv.FormatInt(accountID, 10) }
func browsersConnKey(accountID int64) string {
	return "browsers:" + strconv.FormatInt(accountID, 10)
}
func sessionConnKey(hash string) string { return "session:" + hash }
func tokenConnKey(id int64) string      { return "token:" + strconv.FormatInt(id, 10) }

// labTokenRequest finds the person r's Lab token belongs to and passes them
// on (see tokenUseOf). A use is written down with its time and address, and
// pushes the token's date out; an expired token is passed on as expired and
// is not a use, and neither is the token of a Blocked person.
//
// The body of a request that can change something is read through liveBody,
// held to the token as it is when each part comes: still there, unexpired,
// its person not Blocked and in the role they had when it was let in. A body
// held back across a cut does nothing after it.
func (s *Server) labTokenRequest(r *http.Request) *http.Request {
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(secret, labTokenPrefix) {
		return r
	}
	hash := hashSecret(secret)
	p, t, err := s.store.labTokenByHash(hash)
	if err != nil {
		return r
	}
	s.fillIn(&p)
	now := s.now()
	u := &tokenUse{Person: p, token: t, expired: t.expired(now)}
	if !u.expired && !p.Blocked {
		if addr := callerAddr(r); t.LastUsed.IsZero() || addr != t.Address || now.Sub(t.LastUsed) >= labTokenTouch {
			if err := s.store.useLabToken(t, now, addr); err != nil {
				s.log.Printf("Lab token %q of %s: %v", t.Name, p.Handle, err)
			}
		}
		if !t.told {
			s.tellOfServerMintedToken(p, t, now)
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = &liveBody{ReadCloser: r.Body, live: func() *bodyCut { return s.tokenCut(hash, t.ID, p) }}
			}
		}
	}
	return r.WithContext(context.WithValue(r.Context(), tokenUseKey{}, u))
}

// tokenCut is nil while the token with this hash is still the one let in as
// id, unexpired, and its person neither Blocked nor in another role than
// was's; otherwise it is why not.
func (s *Server) tokenCut(hash string, id int64, was Person) *bodyCut {
	p, t, err := s.store.labTokenByHash(hash)
	switch {
	case err != nil || t.ID != id:
		return &bodyCut{http.StatusUnauthorized, "refused: the Lab token that asked was revoked while its request was on the way, and what it sent was not done"}
	case p.Blocked:
		return &bodyCut{http.StatusForbidden, blockedSentence}
	case t.expired(s.now()):
		return &bodyCut{http.StatusUnauthorized, "refused: the Lab token that asked expired while its request was on the way, and what it sent was not done"}
	}
	s.fillIn(&p)
	if p.Role() != was.Role() {
		return roleCut(p)
	}
	return nil
}

// roleCut is why a body of p's was cut when p's role changed on its way.
func roleCut(p Person) *bodyCut {
	return &bodyCut{http.StatusForbidden, fmt.Sprintf("refused: @%s's role in the lab changed while the request was on the way, and what it sent was not done", p.Handle)}
}

// tellOfServerMintedToken writes what a token made on a page has from the
// start: its line of the People record and its line for "Agents". The
// break-glass command runs beside labd and reaches neither, so a token it
// minted is told of here, at its first use.
func (s *Server) tellOfServerMintedToken(p Person, t labToken, now time.Time) {
	first, err := s.store.markLabTokenTold(t.ID)
	if err != nil || !first {
		return
	}
	if err := s.store.addRecord(recordLine{At: now, ActorID: p.AccountID, Actor: p.Handle, What: fmt.Sprintf("first used the Lab token %q, minted on the server", t.Name)}); err != nil {
		s.log.Printf("the People record: %v", err)
	}
	s.agents.LabTokenMade(p.Handle, t.Name, expiry(t.Period, now))
}

// labTokenName is name as a token is called: one line of up to 64
// characters, with something to read in it.
func labTokenName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 || !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == '"' }) {
		return "", false
	}
	return name, true
}

// errTooManyTokens is the eleventh token.
var errTooManyTokens = fmt.Errorf("has %d Lab tokens, which is the most a person has: revoke one first", labTokenCap)

// errSessionEnded is a token asked for, or a body sent, by a browser session
// that was ended while its request was on the way.
var errSessionEnded = errors.New("the browser session that asked has ended")

// makeLabToken makes a Lab token for the person and returns its secret,
// which nothing keeps. session is the hash of the browser session that asks:
// the token is made only while that session is the person's, in the one
// statement, so a request let in before a cut makes nothing after it. The
// break-glass command has no session and gives none.
func (st *store) makeLabToken(accountID int64, name string, period int, now time.Time, told bool, session string) (secret string, t labToken, err error) {
	secret = newLabToken()
	t = labToken{AccountID: accountID, Name: name, Period: period, Made: now, Expires: expiry(period, now), told: told}
	var expires any
	if !t.Expires.IsZero() {
		expires = t.Expires.UnixMicro()
	}
	res, err := st.db.Exec(`INSERT INTO lab_tokens (hash, account_id, name, period, made, expires, told)
		SELECT ?1, ?2, ?3, ?4, ?5, ?6, ?7 WHERE (SELECT count(*) FROM lab_tokens WHERE account_id = ?2) < ?8
		AND (?9 = '' OR EXISTS (SELECT 1 FROM browser_sessions WHERE hash = ?9 AND account_id = ?2))`,
		hashSecret(secret), accountID, name, period, now.UnixMicro(), expires, told, labTokenCap, session)
	if err != nil {
		return "", t, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if session != "" {
			if _, _, err := st.session(session); err != nil {
				return "", t, errSessionEnded
			}
		}
		return "", t, errTooManyTokens
	}
	t.ID, _ = res.LastInsertId()
	return secret, t, nil
}

const labTokenColumns = `t.id, t.account_id, t.name, t.period, t.made, COALESCE(t.expires, 0), COALESCE(t.last_used, 0), t.last_used_address, t.told`

func scanLabToken(scan func(...any) error, more ...any) (t labToken, err error) {
	var made, expires, used int64
	if err = scan(append([]any{&t.ID, &t.AccountID, &t.Name, &t.Period, &made, &expires, &used, &t.Address, &t.told}, more...)...); err != nil {
		return t, err
	}
	t.Made = time.UnixMicro(made)
	if expires != 0 {
		t.Expires = time.UnixMicro(expires)
	}
	if used != 0 {
		t.LastUsed = time.UnixMicro(used)
	}
	return t, nil
}

// labTokenByHash is the token with this hash and the person it belongs to.
func (st *store) labTokenByHash(hash string) (p Person, t labToken, err error) {
	t, err = scanLabToken(st.db.QueryRow(`SELECT `+labTokenColumns+`, p.handle, p.maintainer, p.m_team, p.leave_to_lend, p.blocked_at IS NOT NULL FROM lab_tokens t
		JOIN people p ON p.account_id = t.account_id WHERE t.hash = ?`, hash).Scan, &p.Handle, &p.Maintainer, &p.MTeam, &p.Lend, &p.Blocked)
	p.AccountID = t.AccountID
	return p, t, err
}

// labTokens are the person's tokens, oldest first.
func (st *store) labTokens(accountID int64) ([]labToken, error) {
	rows, err := st.db.Query(`SELECT `+labTokenColumns+` FROM lab_tokens t WHERE t.account_id = ? ORDER BY t.id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []labToken
	for rows.Next() {
		t, err := scanLabToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// labToken is the person's token with this id; sql.ErrNoRows when it is not
// theirs or not there.
func (st *store) labToken(accountID, id int64) (labToken, error) {
	return scanLabToken(st.db.QueryRow(`SELECT `+labTokenColumns+` FROM lab_tokens t WHERE t.account_id = ? AND t.id = ?`, accountID, id).Scan)
}

// useLabToken writes a use down: its time and address, in place of the last
// one's, and the date the token now expires on.
func (st *store) useLabToken(t labToken, now time.Time, address string) error {
	var expires any
	if e := expiry(t.Period, now); !e.IsZero() {
		expires = e.UnixMicro()
	}
	if _, err := st.db.Exec(`UPDATE lab_tokens SET last_used = ?, last_used_address = ?, expires = ? WHERE id = ?`, now.UnixMicro(), address, expires, t.ID); err != nil {
		return err
	}
	_, err := st.db.Exec(`UPDATE people SET last_seen = ? WHERE account_id = ?`, now.UnixMicro(), t.AccountID)
	return err
}

// renewLabToken makes an expired token of the person's work again, for its
// period from now; it reports whether there was one to renew.
func (st *store) renewLabToken(t labToken, now time.Time) (bool, error) {
	res, err := st.db.Exec(`UPDATE lab_tokens SET expires = ? WHERE id = ? AND account_id = ? AND expires IS NOT NULL AND expires <= ?`,
		expiry(t.Period, now).UnixMicro(), t.ID, t.AccountID, now.UnixMicro())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (st *store) markLabTokenTold(id int64) (first bool, err error) {
	res, err := st.db.Exec(`UPDATE lab_tokens SET told = 1 WHERE id = ? AND told = 0`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (st *store) removeLabToken(accountID, id int64) error {
	_, err := st.db.Exec(`DELETE FROM lab_tokens WHERE account_id = ? AND id = ?`, accountID, id)
	return err
}

func (st *store) countSessions(accountID int64, now time.Time) (n int, err error) {
	err = st.db.QueryRow(`SELECT count(*) FROM browser_sessions WHERE account_id = ? AND last_used >= ?`, accountID, now.Add(-sessionIdle).UnixMicro()).Scan(&n)
	return n, err
}

func (st *store) endSessions(accountID int64) error {
	_, err := st.db.Exec(`DELETE FROM browser_sessions WHERE account_id = ?`, accountID)
	return err
}

// endAllAccess removes every browser session and Lab token of the person.
func (st *store) endAllAccess(accountID int64) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM browser_sessions WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM lab_tokens WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// person is the Person labd keeps under this account id.
func (st *store) person(accountID int64) (p Person, err error) {
	err = st.db.QueryRow(`SELECT account_id, handle, maintainer FROM people WHERE account_id = ?`, accountID).Scan(&p.AccountID, &p.Handle, &p.Maintainer)
	return p, err
}

// peopleCalled are the people labd keeps under this handle, whatever its
// case: more than one while a renamed account has not signed in again.
func (st *store) peopleCalled(handle string) ([]Person, error) {
	rows, err := st.db.Query(`SELECT account_id, handle, maintainer FROM people WHERE handle = ? COLLATE NOCASE ORDER BY account_id`, handle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.AccountID, &p.Handle, &p.Maintainer); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// revokeLabToken is the first cut: the token stops on the next request and
// its open streams end. Nothing of it is kept, the address of its last use
// included.
func (s *Server) revokeLabToken(t labToken) error {
	if err := s.store.removeLabToken(t.AccountID, t.ID); err != nil {
		return err
	}
	s.conns.end(tokenConnKey(t.ID))
	return nil
}

// signOutEverywhere ends every browser session of the person, and nothing
// else: their Lab tokens go on.
func (s *Server) signOutEverywhere(accountID int64) error {
	if err := s.store.endSessions(accountID); err != nil {
		return err
	}
	s.conns.end(browsersConnKey(accountID))
	return nil
}

// endAllAccess is the second cut: every browser session, Lab token, unused
// Join command and Lease of the person ends, the Leases as when a
// Maintainer ends them. by used it: the person, or a Maintainer for them.
// The person stays, and signs in again.
func (s *Server) endAllAccess(p, by Person) error {
	if err := s.store.endAllAccess(p.AccountID); err != nil {
		return err
	}
	s.conns.end(accountConnKey(p.AccountID))
	s.endJoinCommandsOf(p, by, false)
	end := allAccessEnded(by)
	end.note = "ended: @" + by.Handle + ` used "End all access"`
	if by.AccountID != p.AccountID {
		end.note += " on @" + p.Handle
	}
	s.endLeasesOf(p, end)
	return nil
}

// Minted is a Lab token the break-glass command made.
type Minted struct {
	Token     string
	Name      string
	Handle    string
	AccountID int64
	Expires   time.Time
}

// BreakGlass mints a Lab token for a named person by writing to the labd.db
// in dataDir, as the command run on the server itself does: whoever can
// write that file, and nobody labd answers. The person is one the lab knows
// by handle; or accountID names them, and a person the lab does not know
// yet is kept under that id and handle, as their first sign-in would. The
// token does what the person may do: for an Admin, labd's configuration
// names the account id.
func BreakGlass(dataDir, handle string, accountID int64, now time.Time) (*Minted, error) {
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if !reHandle.MatchString(handle) {
		return nil, fmt.Errorf("%q is not a GitHub handle", handle)
	}
	path := filepath.Join(dataDir, "labd.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no labd.db in %s: name labd's data directory with --data", dataDir)
	}
	st, err := openStore(path)
	if err != nil {
		return nil, err
	}
	defer st.db.Close()
	var p Person
	switch {
	case accountID > 0:
		p, err = st.person(accountID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := st.signedInAs(accountID, handle, now); err != nil {
				return nil, err
			}
			p = Person{AccountID: accountID, Handle: handle}
		case err != nil:
			return nil, err
		case !strings.EqualFold(p.Handle, handle):
			return nil, fmt.Errorf("account %d is @%s in this lab, not @%s", accountID, p.Handle, handle)
		}
	default:
		called, err := st.peopleCalled(handle)
		switch {
		case err != nil:
			return nil, err
		case len(called) == 0:
			return nil, fmt.Errorf("the lab knows nobody as @%s: for a person who has not signed in yet, give their GitHub account id with --account-id", handle)
		case len(called) > 1:
			return nil, fmt.Errorf("the lab knows %d accounts as @%s: say which with --account-id", len(called), handle)
		}
		p = called[0]
	}
	name := "break-glass " + now.UTC().Format("2006-01-02 15:04:05")
	secret, t, err := st.makeLabToken(p.AccountID, name, breakGlassPeriod, now, false, "")
	if errors.Is(err, errTooManyTokens) {
		return nil, fmt.Errorf("@%s %v", p.Handle, err)
	}
	if err != nil {
		return nil, err
	}
	return &Minted{Token: secret, Name: t.Name, Handle: p.Handle, AccountID: p.AccountID, Expires: t.Expires}, nil
}

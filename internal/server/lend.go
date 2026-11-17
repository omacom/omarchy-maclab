package server

// Lending a Mac: leave to lend, the cap, a Mac's name and the Join command.
//
// A person needs leave to lend once: the M team and Maintainers have it, an
// Invite gives it, and anyone else sends a Request to lend that a Maintainer
// answers. With leave, the person names a Mac on "Lend a Mac" and gets a
// Join command: one line carrying an enrollment token good for one Mac,
// once, for an hour. Nobody makes one for another person's Mac; a
// Maintainer's `lab enroll` makes one for a Lab-owned Mac only.
//
// Each event writes its line of the People record and one for the Lab
// group, which names the Maintainer who acted and never the person asking.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/omacom/omarchy-maclab/catalogue"
	"github.com/omacom/omarchy-maclab/internal/api"
)

const (
	// lendCap is how many Macs a person lends, an unused Join command
	// counting as one, unless a Maintainer raised theirs. Maintainers have
	// no cap.
	lendCap = 5
	// lendCapMost bounds what a Maintainer types in the cap control.
	lendCapMost = 99
	// joinCommandTTL is how long a Join command's token lasts.
	joinCommandTTL = time.Hour
)

// lendModel is one Mac of the site's list of models.
type lendModel struct {
	ID, Name string
	// Portable is a Mac with a battery: a laptop to the lab.
	Portable bool
}

var lendModelList = sync.OnceValue(func() []lendModel {
	var c struct {
		Boards map[string]struct {
			Name     string `json:"name"`
			Hardware struct {
				Battery bool `json:"battery"`
			} `json:"hardware"`
		} `json:"boards"`
	}
	if err := json.Unmarshal(catalogue.JSON, &c); err != nil {
		panic("the catalogue's boards: " + err.Error())
	}
	all := make([]lendModel, 0, len(c.Boards))
	for id, b := range c.Boards {
		all = append(all, lendModel{ID: id, Name: b.Name, Portable: b.Hardware.Battery})
	}
	sort.Slice(all, func(i, k int) bool {
		if all[i].Name != all[k].Name {
			return all[i].Name < all[k].Name
		}
		return all[i].ID < all[k].ID
	})
	// Two boards can be one model to its owner: the list names it once.
	var out []lendModel
	for _, m := range all {
		if len(out) == 0 || out[len(out)-1].Name != m.Name {
			out = append(out, m)
		}
	}
	return out
})

// lendModels is the site's list of Mac models, by name.
func lendModels() []lendModel { return lendModelList() }

func lendModelByID(id string) (lendModel, bool) {
	for _, m := range lendModels() {
		if m.ID == id {
			return m, true
		}
	}
	return lendModel{}, false
}

// Request states.
const (
	requestWaiting   = "waiting"
	requestWithdrawn = "withdrawn"
	requestApproved  = "approved"
	requestDeclined  = "declined"
)

// requestToLend is one person asking for leave to lend.
type requestToLend struct {
	ID         int64
	AccountID  int64
	Handle     string
	Model      string // the model's id in the site's list
	Note       string
	Contact    string
	Asked      time.Time
	State      string
	Answered   time.Time
	AnsweredBy int64
	Reason     string
}

// ModelName is the request's model as the site's list names it.
func (q requestToLend) ModelName() string {
	if m, ok := lendModelByID(q.Model); ok {
		return m.Name
	}
	return q.Model
}

const requestColumns = `q.id, q.account_id, p.handle, q.model, q.note, q.contact, q.asked, q.state, COALESCE(q.answered, 0), COALESCE(q.answered_by, 0), q.reason`

func scanRequest(scan func(...any) error) (q requestToLend, err error) {
	var asked, answered int64
	if err = scan(&q.ID, &q.AccountID, &q.Handle, &q.Model, &q.Note, &q.Contact, &asked, &q.State, &answered, &q.AnsweredBy, &q.Reason); err != nil {
		return q, err
	}
	q.Asked = time.UnixMicro(asked)
	if answered != 0 {
		q.Answered = time.UnixMicro(answered)
	}
	return q, nil
}

// lastRequest is the person's newest Request to lend; sql.ErrNoRows when
// they never sent one.
func (st *store) lastRequest(accountID int64) (requestToLend, error) {
	return scanRequest(st.db.QueryRow(`SELECT `+requestColumns+` FROM requests_to_lend q JOIN people p ON p.account_id = q.account_id
		WHERE q.account_id = ? ORDER BY q.id DESC LIMIT 1`, accountID).Scan)
}

// waitingRequests are the Requests to lend no Maintainer has answered,
// oldest first.
func (st *store) waitingRequests() ([]requestToLend, error) {
	rows, err := st.db.Query(`SELECT ` + requestColumns + ` FROM requests_to_lend q JOIN people p ON p.account_id = q.account_id
		WHERE q.state = 'waiting' ORDER BY q.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []requestToLend
	for rows.Next() {
		q, err := scanRequest(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (st *store) countWaitingRequests() (n int, err error) {
	err = st.db.QueryRow(`SELECT count(*) FROM requests_to_lend WHERE state = 'waiting'`).Scan(&n)
	return n, err
}

// errRequestWaiting is a second Request to lend while one waits.
var errRequestWaiting = errors.New("a Request to lend is waiting already")

func (st *store) addRequest(q requestToLend) error {
	_, err := st.db.Exec(`INSERT INTO requests_to_lend (account_id, model, note, contact, asked, state) VALUES (?, ?, ?, ?, ?, 'waiting')`,
		q.AccountID, q.Model, q.Note, q.Contact, q.Asked.UnixMicro())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errRequestWaiting
	}
	return err
}

// settleRequest ends the person's waiting request as state, and says
// whether one was waiting.
func (st *store) settleRequest(accountID int64, state string, by int64, reason string, now time.Time) (bool, error) {
	res, err := st.db.Exec(`UPDATE requests_to_lend SET state = ?, answered = ?, answered_by = ?, reason = ? WHERE account_id = ? AND state = 'waiting'`,
		state, now.UnixMicro(), by, reason, accountID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// lendCapOf is the cap a Maintainer raised for the person, 0 for none.
func (st *store) lendCapOf(accountID int64) (int, error) {
	var n sql.NullInt64
	err := st.db.QueryRow(`SELECT lend_cap FROM people WHERE account_id = ?`, accountID).Scan(&n)
	return int(n.Int64), err
}

func (st *store) setLendCap(accountID int64, macs int) error {
	_, err := st.db.Exec(`UPDATE people SET lend_cap = ? WHERE account_id = ?`, macs, accountID)
	return err
}

// joinCommand is a Join command as the lab keeps it: never its token.
type joinCommand struct {
	Name        string
	MadeBy      int64
	VolunteerID int64
	Expires     time.Time
}

// unusedJoinCommand is the Join command the person made for a Mac of their
// own that no Mac has enrolled with, and that has not expired.
func (st *store) unusedJoinCommand(accountID int64, now time.Time) (*joinCommand, error) {
	return scanJoinCommand(st.db.QueryRow(unusedOf, accountID, now.Unix()).Scan)
}

const (
	joinCommandColumns = `t.name, j.made_by, j.volunteer_id, t.expires`
	unusedOf           = `SELECT ` + joinCommandColumns + ` FROM tokens t JOIN join_commands j ON j.token_hash = t.token_hash
		WHERE j.volunteer_id = ? AND t.activated_by = '' AND t.expires >= ? ORDER BY t.expires DESC LIMIT 1`
	// dropUnusedOf drops a person's unused Join commands, and dropOrphans
	// what was kept of tokens that are gone: used up, revoked or dropped.
	dropUnusedOf = `DELETE FROM tokens WHERE activated_by = '' AND token_hash IN (SELECT token_hash FROM join_commands WHERE volunteer_id = ?)`
	// unusedMadeBy are the unused Join commands an account made: all of
	// them, or with the last argument 1 only those for Lab-owned Macs.
	unusedMadeBy = ` FROM tokens t JOIN join_commands j ON j.token_hash = t.token_hash
		WHERE j.made_by = ? AND t.activated_by = '' AND (? = 0 OR j.volunteer_id = 0)`
	dropOrphans = `DELETE FROM join_commands WHERE token_hash NOT IN (SELECT token_hash FROM tokens)`
)

// scanJoinCommand is the Join command a row holds, nil when there is none.
func scanJoinCommand(scan func(...any) error) (*joinCommand, error) {
	var c joinCommand
	var exp int64
	switch err := scan(&c.Name, &c.MadeBy, &c.VolunteerID, &exp); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	c.Expires = time.Unix(exp, 0)
	return &c, nil
}

// beforeJoinCommandStored, when set, runs as a Join command is about to be
// stored; tests cut its asker there.
var beforeJoinCommandStored func()

// asker is the credential a request came with: a browser session's hash or
// a Lab token's id, both zero for neither.
type asker struct {
	session string
	token   int64
}

func askerOf(r *http.Request) asker {
	if u := tokenUseOf(r); u != nil {
		return asker{token: u.token.ID}
	}
	if si := signedInOf(r); si != nil {
		return asker{session: si.hash}
	}
	return asker{}
}

// errAskerCut is a Join command whose asker's credential was cut, or whose
// person was Blocked, while the request was on the way.
var errAskerCut = errors.New("refused: the access that asked for this Join command was cut while the request was on the way")

// errNameHeld is a Join command asked for a name another unused one holds.
var errNameHeld = errors.New("another Join command holds the name")

// addJoinCommand stores a Join command's token. One a person makes for
// their own Mac takes the place of the unused one they had, which it
// returns; one for a Lab-owned Mac stands beside the Maintainer's others.
// A name another person's unused Join command holds is errNameHeld.
func (st *store) addJoinCommand(hash string, req api.EnrollTokenRequest, madeBy int64, now, exp time.Time) (cancelled *joinCommand, err error) {
	return st.addJoinCommandAsked(hash, req, madeBy, asker{}, now, exp)
}

// addJoinCommandAsked is addJoinCommand for a request that came with the
// credential by. The command is stored only while that is still there and
// its person not Blocked, in the same transaction: a cut that came after the
// request was let in either finds the command and sweeps it, or comes first
// and the command is not stored (errAskerCut).
func (st *store) addJoinCommandAsked(hash string, req api.EnrollTokenRequest, madeBy int64, by asker, now, exp time.Time) (cancelled *joinCommand, err error) {
	if beforeJoinCommandStored != nil {
		beforeJoinCommandStored()
	}
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var live bool
	if err := tx.QueryRow(`SELECT (?1 = '' AND ?2 = 0) OR (NOT EXISTS (SELECT 1 FROM people WHERE account_id = ?3 AND blocked_at IS NOT NULL)
		AND (EXISTS (SELECT 1 FROM browser_sessions WHERE hash = ?1 AND ?1 != '' AND account_id = ?3)
		OR EXISTS (SELECT 1 FROM lab_tokens WHERE id = ?2 AND ?2 != 0 AND account_id = ?3)))`, by.session, by.token, madeBy).Scan(&live); err != nil {
		return nil, err
	}
	if !live {
		return nil, errAskerCut
	}
	if req.VolunteerID != 0 {
		if cancelled, err = scanJoinCommand(tx.QueryRow(unusedOf, req.VolunteerID, now.Unix()).Scan); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(dropUnusedOf, req.VolunteerID); err != nil {
			return nil, err
		}
	}
	var held int
	if err := tx.QueryRow(`SELECT count(*) FROM tokens t LEFT JOIN join_commands j ON j.token_hash = t.token_hash
		WHERE t.name = ? AND t.activated_by = '' AND t.expires >= ? AND (COALESCE(j.volunteer_id, 0) != 0 OR ? != 0)`,
		req.Name, now.Unix(), req.VolunteerID).Scan(&held); err != nil {
		return nil, err
	}
	if held > 0 {
		return nil, errNameHeld
	}
	if _, err := tx.Exec(`INSERT INTO tokens(token_hash,name,label,model,volunteer,requester,volunteer_name,contact,volunteer_login,expires)
		VALUES(?,?,?,?,?,'','','','',?)`, hash, req.Name, req.Label, req.Model, req.Volunteer, exp.Unix()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(dropOrphans); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO join_commands(token_hash, made_by, made, volunteer_id) VALUES(?,?,?,?)`, hash, madeBy, now.UnixMicro(), req.VolunteerID); err != nil {
		return nil, err
	}
	return cancelled, tx.Commit()
}

// cancelJoinCommand drops the person's unused Join command and returns it,
// nil when they had none.
func (st *store) cancelJoinCommand(accountID int64, now time.Time) (*joinCommand, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c, err := scanJoinCommand(tx.QueryRow(unusedOf, accountID, now.Unix()).Scan)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(dropUnusedOf, accountID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(dropOrphans); err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

// cancelJoinCommandsMadeBy drops the unused Join commands the account made,
// for Lab-owned Macs only when labOwned is set, and returns those that had
// not run out.
func (st *store) cancelJoinCommandsMadeBy(accountID int64, labOwned bool, now time.Time) ([]joinCommand, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+joinCommandColumns+unusedMadeBy+` AND t.expires >= ? ORDER BY t.name`, accountID, labOwned, now.Unix())
	if err != nil {
		return nil, err
	}
	var out []joinCommand
	for rows.Next() {
		c, err := scanJoinCommand(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM tokens WHERE token_hash IN (SELECT t.token_hash`+unusedMadeBy+`)`, accountID, labOwned); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(dropOrphans); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// unusedTokensFor are the unused, unexpired Join commands for a name.
func (st *store) unusedTokensFor(name string, now time.Time) (n int, err error) {
	err = st.db.QueryRow(`SELECT count(*) FROM tokens WHERE name = ? AND activated_by = '' AND expires >= ?`, name, now.Unix()).Scan(&n)
	return n, err
}

// expireJoinCommands drops the Join commands that ran out unused and
// returns them. A token from before Join commands (made 0) goes unsaid.
func (st *store) expireJoinCommands(now time.Time) ([]joinCommand, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	const ranOut = ` FROM tokens t JOIN join_commands j ON j.token_hash = t.token_hash WHERE t.activated_by = '' AND t.expires < ? AND j.made != 0`
	rows, err := tx.Query(`SELECT `+joinCommandColumns+ranOut+` ORDER BY t.expires, t.name`, now.Unix())
	if err != nil {
		return nil, err
	}
	var out []joinCommand
	for rows.Next() {
		c, err := scanJoinCommand(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		if _, err := tx.Exec(`DELETE FROM tokens WHERE token_hash IN (SELECT t.token_hash`+ranOut+`)`, now.Unix()); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(dropOrphans); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// bindToken ties an unbound token to the Mac whose activation key hashes to
// keyHash, and says whether the token is that Mac's now: the first Mac to
// use a Join command is the only one it serves.
func (st *store) bindToken(hash, keyHash string) (bool, error) {
	if _, err := st.db.Exec(`INSERT INTO join_commands(token_hash, bound_to) SELECT token_hash, ? FROM tokens WHERE token_hash = ?
		ON CONFLICT(token_hash) DO UPDATE SET bound_to = excluded.bound_to WHERE join_commands.bound_to = ''`, keyHash, hash); err != nil {
		return false, err
	}
	var bound string
	err := st.db.QueryRow(`SELECT bound_to FROM join_commands WHERE token_hash = ?`, hash).Scan(&bound)
	return err == nil && bound == keyHash, err
}

// typedLine is what a person typed in a one-line field, trimmed; ok is
// false when it is longer than most runes or has a control character.
func typedLine(raw string, most int) (string, bool) {
	line := strings.TrimSpace(raw)
	if len([]rune(line)) > most || strings.ContainsFunc(line, unicode.IsControl) {
		return "", false
	}
	return line, true
}

// lends is what counts toward a person's cap now.
type lends struct {
	Macs []string
	// Unused is the person's unused Join command, nil when they have none.
	Unused *joinCommand
	// Cap is 0 for a Maintainer, who has none.
	Cap int
}

// Count is the Macs the person lends, an unused Join command among them.
func (l lends) Count() int {
	if l.Unused != nil {
		return len(l.Macs) + 1
	}
	return len(l.Macs)
}

// lendsOf is what p lends and the cap it counts toward.
func (s *Server) lendsOf(p Person) (lends, error) {
	l := lends{Macs: s.macsLentBy(p.AccountID)}
	var err error
	if l.Unused, err = s.store.unusedJoinCommand(p.AccountID, s.now()); err != nil {
		return l, err
	}
	if p.isMaintainer() {
		return l, nil
	}
	l.Cap = lendCap
	if raised, err := s.store.lendCapOf(p.AccountID); err != nil {
		return l, err
	} else if raised > l.Cap {
		l.Cap = raised
	}
	return l, nil
}

// requestToLend sends the Request to lend of the person signed in on r.
func (s *Server) requestToLend(r *http.Request, model, note, contact string) error {
	p := PersonOf(r)
	if p.MayLend() {
		return refuse(http.StatusConflict, "You have leave to lend already.")
	}
	if _, ok := lendModelByID(model); !ok {
		return refuse(http.StatusUnprocessableEntity, "Choose the Mac's model from the list.")
	}
	note, ok := typedLine(note, 500)
	if !ok {
		return refuse(http.StatusUnprocessableEntity, "A note is up to 500 characters on one line.")
	}
	if contact, ok = typedLine(contact, 200); !ok {
		return refuse(http.StatusUnprocessableEntity, "A contact is up to 200 characters on one line.")
	}
	err := s.store.addRequest(requestToLend{AccountID: p.AccountID, Model: model, Note: note, Contact: contact, Asked: s.now()})
	if errors.Is(err, errRequestWaiting) {
		return refuse(http.StatusConflict, "Your Request to lend is waiting already.")
	} else if err != nil {
		return err
	}
	s.labGroup.RequestToLendMade()
	return s.record(r, "sent a Request to lend", nil)
}

// withdrawRequest takes back the waiting Request to lend of the person
// signed in on r.
func (s *Server) withdrawRequest(r *http.Request) error {
	p := PersonOf(r)
	ok, err := s.store.settleRequest(p.AccountID, requestWithdrawn, p.AccountID, "", s.now())
	if err != nil {
		return err
	}
	if !ok {
		return refuse(http.StatusConflict, "No Request to lend of yours is waiting.")
	}
	s.labGroup.RequestToLendWithdrawn()
	return s.record(r, "withdrew their Request to lend", nil)
}

// answerRequest is a Maintainer approving or declining k's waiting Request
// to lend. Approval is of the person: it gives leave to lend, which stands
// when their last Mac leaves.
func (s *Server) answerRequest(r *http.Request, k knownPerson, approve bool, reason string) error {
	if k.Blocked {
		return unblockFirst(k)
	}
	reason, ok := typedLine(reason, 200)
	if !ok {
		return refuse(http.StatusUnprocessableEntity, "A reason is one line of up to 200 characters.")
	}
	by := PersonOf(r)
	state := requestDeclined
	if approve {
		state, reason = requestApproved, ""
	}
	settled, err := s.store.settleRequest(k.AccountID, state, by.AccountID, reason, s.now())
	if err != nil {
		return err
	}
	if !settled {
		return refuse(http.StatusConflict, "No Request to lend of @%s is waiting.", k.Handle)
	}
	if approve {
		if err := s.store.setMark(k.AccountID, "leave_to_lend", true); err != nil {
			return err
		}
		s.changed(k.AccountID)
	}
	s.agents.RequestToLendAnswered(by.Handle, k.Handle, approve)
	s.labGroup.RequestToLendAnswered(by.Handle, approve)
	if approve {
		return s.record(r, "approved the Request to lend of", &k.Person)
	}
	return s.recordNoted(r, "declined the Request to lend of", &k.Person, reason)
}

// raiseCap is a Maintainer raising one person's cap.
func (s *Server) raiseCap(r *http.Request, k knownPerson, macs int) error {
	switch {
	case k.Blocked:
		return unblockFirst(k)
	case k.isMaintainer():
		return refuse(http.StatusConflict, "@%s is a Maintainer, and a Maintainer has no cap.", k.Handle)
	}
	had, err := s.lendsOf(k.Person)
	if err != nil {
		return err
	}
	if macs <= had.Cap || macs > lendCapMost {
		return refuse(http.StatusUnprocessableEntity, "@%s's cap is %d Macs: raise it to a number from %d to %d.", k.Handle, had.Cap, had.Cap+1, lendCapMost)
	}
	if err := s.store.setLendCap(k.AccountID, macs); err != nil {
		return err
	}
	s.labGroup.CapRaised(PersonOf(r).Handle, macs)
	return s.record(r, fmt.Sprintf("raised the cap to %d Macs for", macs), &k.Person)
}

// macName is a Mac's name as typed on "Lend a Mac", or why it is none.
func macName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if !reName.MatchString(name) {
		return "", refuse(http.StatusUnprocessableEntity, "A Mac's name is lowercase letters, digits and dashes, up to 31, starting with a letter or a digit.")
	}
	return name, nil
}

// suggestedName is the name the lab suggests for a Mac of this model: the
// model alone, numbered on a clash with a Mac or with an unused Join
// command.
func (s *Server) suggestedName(model lendModel) string {
	base := strings.Trim(reNameRun.ReplaceAllString(strings.ReplaceAll(strings.ToLower(model.Name), "-inch", ""), "-"), "-")
	if base == "" {
		base = "mac"
	}
	for i := 1; i < 100; i++ {
		number := ""
		if i > 1 {
			number = fmt.Sprintf("-%d", i)
		}
		name := strings.TrimRight(base[:min(len(base), 31-len(number))], "-") + number
		if s.nameFree(name) {
			return name
		}
	}
	return ""
}

// nameFree is whether no Mac's record and no unused Join command has the
// name.
func (s *Server) nameFree(name string) bool {
	if s.dev(name) != nil {
		return false
	}
	n, err := s.store.unusedTokensFor(name, s.now())
	return err == nil && n == 0
}

// madeJoinCommand is a Join command just made: the one time its line is
// shown.
type madeJoinCommand struct {
	Name    string
	Line    string
	Expires time.Time
}

// makeJoinCommand makes the Join command of the person signed in on r for a
// Mac of their own, in place of any unused one they had.
func (s *Server) makeJoinCommand(r *http.Request, modelID, rawName string) (*madeJoinCommand, error) {
	p := PersonOf(r)
	if !p.MayLend() {
		return nil, refuse(http.StatusForbidden, "You need leave to lend first: send a Request to lend.")
	}
	model, ok := lendModelByID(modelID)
	if !ok {
		return nil, refuse(http.StatusUnprocessableEntity, "Choose the Mac's model from the list.")
	}
	s.expireJoinCommands()
	name := strings.TrimSpace(rawName)
	if name == "" {
		if name = s.suggestedName(model); name == "" {
			return nil, refuse(http.StatusConflict, "Every name the lab would suggest is taken: type one.")
		}
	} else {
		var err error
		if name, err = macName(name); err != nil {
			return nil, err
		}
	}
	l, err := s.lendsOf(*p)
	if err != nil {
		return nil, err
	}
	if l.Cap != 0 && len(l.Macs) >= l.Cap {
		return nil, refuse(http.StatusConflict, "You lend %d Macs, which is your cap. One has to Leave first, or a Maintainer raises your cap.", len(l.Macs))
	}
	if s.dev(name) != nil {
		return nil, refuse(http.StatusConflict, "A Mac called %s is in the lab already: names are unique.", name)
	}
	req := api.EnrollTokenRequest{Name: name, Label: model.Name, Model: api.ModelDesktop, Volunteer: true, VolunteerID: p.AccountID}
	if model.Portable {
		req.Model = api.ModelLaptop
	}
	token := "enr_" + randHex(16)
	line := s.joinCommand(r, token)
	if line == "" {
		return nil, refuse(http.StatusServiceUnavailable, "This lab has no lab-agent to hand out yet, so it makes no Join command.")
	}
	now := s.now()
	cancelled, err := s.store.addJoinCommandAsked(hashSecret(token), req, p.AccountID, askerOf(r), now, now.Add(joinCommandTTL))
	if errors.Is(err, errNameHeld) {
		return nil, refuse(http.StatusConflict, "A Join command for a Mac called %s is waiting already: names are unique.", name)
	} else if err != nil {
		return nil, err
	}
	if cancelled != nil {
		s.saidOfJoinCommand(r, "cancelled the Join command for "+cancelled.Name)
		s.labGroup.JoinCommandCancelled(cancelled.Name, "")
	}
	s.saidOfJoinCommand(r, "made a Join command for "+name)
	s.labGroup.JoinCommandMade(name, "")
	return &madeJoinCommand{Name: name, Line: line, Expires: now.Add(joinCommandTTL)}, nil
}

// saidOfJoinCommand writes a Join command's line of the People record for
// whoever r comes from; a failed write stops nothing.
func (s *Server) saidOfJoinCommand(r *http.Request, what string) {
	p := credentialOf(r)
	if p == nil {
		return
	}
	if err := s.store.addRecord(recordLine{At: s.now(), ActorID: p.AccountID, Actor: p.Handle, What: what}); err != nil {
		s.log.Printf("the People record: %v", err)
	}
}

// cancelOwnJoinCommand cancels the unused Join command of the person signed
// in on r.
func (s *Server) cancelOwnJoinCommand(r *http.Request) error {
	c, err := s.store.cancelJoinCommand(PersonOf(r).AccountID, s.now())
	if err != nil {
		return err
	}
	if c == nil {
		return refuse(http.StatusConflict, "You have no unused Join command.")
	}
	s.saidOfJoinCommand(r, "cancelled the Join command for "+c.Name)
	s.labGroup.JoinCommandCancelled(c.Name, "")
	return nil
}

// endJoinCommandsOf cancels the unused Join commands p made, so that a line
// made with access that was cut enrolls no Mac: all of them, or with
// labOwned those for Lab-owned Macs, which only a Maintainer makes. by made
// the cut. Each has its line in the People record and for the Lab group.
func (s *Server) endJoinCommandsOf(p, by Person, labOwned bool) {
	gone, err := s.store.cancelJoinCommandsMadeBy(p.AccountID, labOwned, s.now())
	if err != nil {
		s.log.Printf("the Join commands @%s made: %v", p.Handle, err)
		return
	}
	for _, c := range gone {
		l := recordLine{At: s.now(), ActorID: by.AccountID, Actor: by.Handle, What: "cancelled the Join command for " + c.Name}
		if by.AccountID != p.AccountID {
			l.What += " made by"
			l.AboutID, l.About = p.AccountID, p.Handle
		}
		if err := s.store.addRecord(l); err != nil {
			s.log.Printf("the People record: %v", err)
		}
		s.labGroup.JoinCommandCancelled(c.Name, "")
	}
}

// expireJoinCommands records and posts each Join command that ran out
// unused since the last look.
func (s *Server) expireJoinCommands() {
	gone, err := s.store.expireJoinCommands(s.now())
	if err != nil {
		s.log.Printf("expired Join commands: %v", err)
		return
	}
	for _, c := range gone {
		if k, err := s.store.knownPerson(c.MadeBy); err == nil {
			l := recordLine{At: s.now(), ActorID: k.AccountID, Actor: k.Handle, What: "let the Join command for " + c.Name + " expire unused"}
			if err := s.store.addRecord(l); err != nil {
				s.log.Printf("the People record: %v", err)
			}
		}
		s.labGroup.JoinCommandExpired(c.Name)
	}
}

// joinCommandLoop says of each Join command that it expired unused, within
// a minute of its hour.
func (s *Server) joinCommandLoop() {
	t := s.clock().NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
			s.expireJoinCommands()
		}
	}
}

// mayEnroll is why the Mac of token t may not enroll now, or nil: its
// Volunteer is Blocked, lends as many Macs as their cap allows, or the name
// was taken since the Join command was made. A Lab-owned Mac's token asks
// that whoever made it is still a Maintainer, and meets the last.
func (s *Server) mayEnroll(t *tokenState) error {
	if t.VolunteerID != 0 {
		p, err := s.person(t.VolunteerID)
		switch {
		case err != nil:
			return refuse(http.StatusForbidden, "The person who made this Join command is not known to the lab any more.")
		case p.Blocked:
			return refuse(http.StatusForbidden, "%s", blockedSentence)
		case !p.MayLend():
			return refuse(http.StatusForbidden, "The person who made this Join command no longer has leave to lend.")
		}
		if l, err := s.lendsOf(p); err != nil {
			return err
		} else if l.Cap != 0 && len(l.Macs) >= l.Cap {
			return refuse(http.StatusConflict, "Cap reached: you lend %d Macs, which is your cap. One has to Leave first, or a Maintainer raises your cap.", len(l.Macs))
		}
	} else if t.MadeBy != 0 {
		if p, err := s.person(t.MadeBy); err != nil || p.Blocked || !p.isMaintainer() {
			return refuse(http.StatusForbidden, "The person who made this Join command is no longer a Maintainer.")
		}
	}
	if v := s.dev(t.Name); v != nil {
		return refuse(http.StatusConflict, "Name taken: a Mac called %s is in the lab already. Make a new Join command with another name.", t.Name)
	}
	return nil
}

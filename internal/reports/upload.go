package reports

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The upload is the old site's, POST /api/v1/reports, step by step. Each
// step ends the request, in this order:
//
//  1. the address's limit, 429: refused uploads count too
//  2. no Content-Length, 411
//  3. larger than MaxUploadBytes, 413
//  4. not JSON, 400
//  5. to 9. the report's own rules, 422 (Rules.Judge): an outdated schema, no
//     signature, the schema, the signature, check ids the catalogue lacks,
//     the evidence
//  10. the Mac's limit, 429: counted only for a report that would be stored,
//     so nobody uses up another Mac's uploads
//  11. stored, 201, with the report's link and its deletion link
//
// One step is the lab's own, between 9 and 10: a report the lab has stored
// before is refused, 409, with the report's link and never its deletion link.
const (
	// MaxUploadBytes leaves room for everything beside the evidence, which
	// is capped at 64 KiB a report.
	MaxUploadBytes = 256 << 10
	// UploadsPerHour is the limit for one address, UploadsPerHourPerMachine
	// for one Machine key.
	UploadsPerHour           = 30
	UploadsPerHourPerMachine = 10

	limitWindow = time.Hour
)

// Uploads takes reports in. Its counts are in memory, as the old site's
// were: they start again when labd does.
type Uploads struct {
	// OnMTeam reports whether the person with that account id is on the
	// Omarchy M team now: what makes the report of a Mac signed in as them
	// a Verified run. Nil is nobody: every run is a Community run.
	OnMTeam func(accountID int64) bool

	store     *Store
	rules     *Rules
	now       func() time.Time
	byAddress *limiter
	byMachine *limiter
}

func NewUploads(store *Store, rules *Rules, now func() time.Time) *Uploads {
	return &Uploads{store: store, rules: rules, now: now, byAddress: newLimiter(UploadsPerHour), byMachine: newLimiter(UploadsPerHourPerMachine)}
}

// Receive takes a report from its text on: steps 4 to 11. It is the one way
// in for a report; installURL is where the refusals that ask for an update
// send the user. The error is the lab's own failure to store, and comes with
// a refusal of status 500.
func (u *Uploads) Receive(body []byte, installURL string) (*Stored, *Refusal, error) {
	return u.receive(body, installURL, nil)
}

// ReceiveFromLab is Receive for the report of a lab job's own run, handed
// over inside labd: the same steps and the Mac's limit, and no address to
// count. What it adds is the record of how the report arrived. Nothing a
// request carries leads here: an upload is Handle's, whatever it claims.
func (u *Uploads) ReceiveFromLab(body []byte, installURL string, lab Lab) (*Stored, *Refusal, error) {
	if lab.Job == "" {
		return nil, notStored, errors.New("reports: a report from the lab names its job")
	}
	return u.receive(body, installURL, &lab)
}

func (u *Uploads) receive(body []byte, installURL string, lab *Lab) (*Stored, *Refusal, error) {
	judged, refusal := u.rules.Judge(body, installURL)
	if refusal != nil {
		return nil, refusal, nil
	}
	if lab != nil && lab.Machine != "" && judged.MachineID != lab.Machine {
		return nil, &Refusal{Status: http.StatusForbidden, Error: AnotherMachineKey}, nil
	}
	heldAs, held, err := u.store.held(Digest(judged.MachineID, judged.Body))
	if err != nil {
		return nil, notStored, err
	}
	if held {
		return nil, alreadyUploaded(heldAs), nil
	}
	now := u.now()
	if u.byMachine.over(judged.MachineID, now) {
		return nil, &Refusal{Status: http.StatusTooManyRequests,
			Error: fmt.Sprintf("Too many uploads from this Mac: at most %d an hour. Try again later; the report is saved on your Mac.", UploadsPerHourPerMachine)}, nil
	}
	stored, heldAs, repeat, err := u.store.add(judged, now, u.OnMTeam, lab)
	if err != nil {
		return nil, notStored, err
	}
	if repeat {
		return nil, alreadyUploaded(heldAs), nil
	}
	return stored, nil, nil
}

// AnotherMachineKey is why a lab run's report signed by a key other than
// its Mac's is not published.
const AnotherMachineKey = "The report is signed by another Machine key than the Mac's own: it is not this Mac's report."

// SignedBy is the machine id of the Machine key whose valid signature body
// carries; false for a body with none. It stores and counts nothing: the lab
// reads it when it collects a run, to bind the Mac's key at its first report.
func (u *Uploads) SignedBy(body []byte) (string, bool) {
	payload, err := Parse(body)
	if err != nil {
		return "", false
	}
	report, _ := payload.(*Object)
	if report == nil {
		return "", false
	}
	key, err := VerifySignature(report, NamespaceReport)
	if err != nil {
		return "", false
	}
	return u.rules.MachineID(key), true
}

var notStored = &Refusal{Status: http.StatusInternalServerError, Error: "The lab could not store the report. Try again later; the report is saved on your Mac."}

func alreadyUploaded(heldAs string) *Refusal {
	if heldAs == "" {
		return &Refusal{Status: http.StatusConflict, Error: "Already uploaded", Details: []string{"The report was deleted since."}}
	}
	return &Refusal{Status: http.StatusConflict, Error: "Already uploaded", Held: heldAs}
}

// Handle answers POST /api/v1/reports as the old site did. addr is the
// address the upload came from, for its limit; site is the lab's address,
// which the links in the answer and the install line start with. The error
// is for labd's log: the upload has been answered either way.
func (u *Uploads) Handle(w http.ResponseWriter, r *http.Request, addr, site string) error {
	if u.byAddress.over(addr, u.now()) {
		answerJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": fmt.Sprintf("Too many uploads from your network: at most %d an hour. Try again later; the report is saved on your Mac.", UploadsPerHour)})
		return nil
	}
	// A request that names no length and no body reads as length 0.
	if r.ContentLength < 0 || (r.ContentLength == 0 && r.Header.Get("Content-Length") == "") {
		answerJSON(w, http.StatusLengthRequired, map[string]any{"error": "Send the report with a Content-Length."})
		return nil
	}
	if r.ContentLength > MaxUploadBytes {
		answerJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("The report is larger than %d KiB.", MaxUploadBytes>>10)})
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if err != nil || int64(len(body)) != r.ContentLength {
		answerJSON(w, http.StatusBadRequest, map[string]any{"error": "The report is not valid JSON."})
		return nil
	}
	stored, refusal, err := u.Receive(body, site+"/install")
	switch {
	case refusal != nil && refusal.Held != "":
		answerJSON(w, refusal.Status, map[string]any{"error": refusal.Error, "report_url": site + "/reports/" + refusal.Held})
	case refusal != nil:
		answerJSON(w, refusal.Status, refusal)
	default:
		answerJSON(w, http.StatusCreated, map[string]any{
			"id":           stored.PublicID,
			"report_url":   site + "/reports/" + stored.PublicID,
			"deletion_url": site + "/reports/" + stored.PublicID + "/deletion?token=" + stored.DeletionToken,
			// The wire's name for a Verified run.
			"tester": stored.Verified,
		})
	}
	return err
}

func answerJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// limiter counts uploads by key over an hour that starts with the first one
// counted, as the old site's rate limit did.
type limiter struct {
	limit int

	mu     sync.Mutex
	counts map[string]*window
	swept  time.Time
}

type window struct {
	ends  time.Time
	count int
}

func newLimiter(limit int) *limiter {
	return &limiter{limit: limit, counts: map[string]*window{}}
}

// over counts one more upload for key and reports whether that is past the
// limit.
func (l *limiter) over(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) >= limitWindow {
		for k, w := range l.counts {
			if !now.Before(w.ends) {
				delete(l.counts, k)
			}
		}
		l.swept = now
	}
	w := l.counts[key]
	if w == nil || !now.Before(w.ends) {
		w = &window{ends: now.Add(limitWindow)}
		l.counts[key] = w
	}
	w.count++
	return w.count > l.limit
}

// ReadMachineIDKey reads the machine-id key from its file: the 64 bytes as
// 128 hex digits, in a file only its own account can read. No path, or no
// file there, is no key: a lab without one takes no reports.
func ReadMachineIDKey(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the machine-id key: %v", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s can be read by other accounts: chmod 600 it", path)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the machine-id key: %v", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil || len(key) != machineIDKeyBytes {
		return nil, fmt.Errorf("%s is not the machine-id key: want %d bytes as %d hex digits", path, machineIDKeyBytes, 2*machineIDKeyBytes)
	}
	return key, nil
}

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// hExec runs a shell command on a Mac, as root, through lab-agent. This is
// what agents use instead of ssh. Nobody runs it without their own Lease on
// the Mac, a Maintainer included, and the holder's is cut when the Lease
// ends. Every one, and every refused attempt, is in the Exec record. The
// answer is held until the command ends (see held): cutting it does not
// stop the command on the Mac.
func (s *Server) hExec(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	var a api.ExecArgs
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil || a.Command == "" {
		httpErr(w, 400, "send {\"command\": \"...\"}")
		return
	}
	if a.TimeoutSec <= 0 || a.TimeoutSec > 3600 {
		a.TimeoutSec = 120
	}
	p := leaser(r)
	err := s.fence(v, p, fenceExec)
	if err == nil {
		if e := s.mayDo(callerOf(r), v, actExec); e != nil {
			err = refuse(403, "%v", e)
		}
	}
	name := v.snapshot().Name
	if err == nil && !v.alive() {
		err = refuse(409, "%s is offline, and lab exec reaches a Mac only through lab-agent. %s", name, api.NoSerialConsole)
	}
	if err == nil {
		v.mu.Lock()
		err = v.updatingRefusalLocked("this command was not sent")
		v.mu.Unlock()
	}
	// The request is a session of the Lease before the Lease is asked for
	// again: one that ended since the fence passed either shows here or
	// cuts the session.
	session, done := s.underLease(r, v, p)
	defer done()
	ended := func() error {
		return refuse(409, "your Lease on %s ended as this command arrived, so it was not sent: `lab lease %s`", name, name)
	}
	if err == nil && (session == r || !s.holdsLease(v, p)) {
		err = ended()
	}
	if err != nil {
		s.execRefused(v, p, a.Command, err)
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	record, err := s.execSent(v, p, a.Command)
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	if s.execRecorded != nil {
		s.execRecorded()
	}
	// Writing the line took time, and the Lease is asked for a last time in
	// the moment the command is queued: the Mac's agent takes nothing that
	// was queued after the Lease ended, ran out under its job, or the
	// session was cut. sendIf asks it holding v.mu.
	stillLeased := func() error {
		switch l := v.liveLeaseLocked(); {
		case r.Context().Err() != nil:
			return errNotSent{errors.New("its caller left before it was sent")}
		case v.updating != nil:
			return errNotSent{v.updatingRefusalLocked("this command was not sent")}
		case l != nil && leaseIsOf(l, p) && l.Extended:
			return errNotSent{refuse(409, "%s, so this command was not sent", extendedOnly(l, name, fenceExec.what))}
		case l == nil || !leaseIsOf(l, p) || session.Context().Err() != nil:
			return errNotSent{ended()}
		}
		return nil
	}
	s.held(w, r, func() (any, error) {
		res := api.ExecResult{Via: "agent"}
		if err := v.callIf(session.Context(), api.CmdExec, "", a, time.Duration(a.TimeoutSec+30)*time.Second, &res, stillLeased); err != nil {
			switch {
			case errors.As(err, new(errNotSent)):
			case r.Context().Err() != nil:
				err = errors.New("its caller left before the Mac answered")
			default:
				err = leaseCut(session, r, v, err)
			}
			record(nil, err)
			return nil, err
		}
		res.Via = "agent"
		record(&res, nil)
		return res, nil
	})
}

// leaseCut is err, or, when the session ended because its Lease did and
// not because its caller left, the refusal that says so.
func leaseCut(session, r *http.Request, v *dev, err error) error {
	if session.Context().Err() != nil && r.Context().Err() == nil {
		return refuse(409, "the Lease on %s ended, which cut this session; a command it started may still be running on the Mac", v.snapshot().Name)
	}
	return err
}

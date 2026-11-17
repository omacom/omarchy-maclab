package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func effectiveRecoveryMode(requested, inherited string) string {
	if requested != "" {
		return requested
	}
	if inherited == api.RecoveryNetworkOnly {
		return inherited
	}
	return ""
}
func (v *dev) networkOnlyLocked() bool { return v.d.EffectiveRecoveryMode == api.RecoveryNetworkOnly }
func (v *dev) alive() bool             { v.mu.Lock(); defer v.mu.Unlock(); return v.aliveLocked() }
func (r *jobRun) networkOnly() bool {
	return r.j.EffectiveRecoveryMode == api.RecoveryNetworkOnly || r.j.Spec.RecoveryMode == api.RecoveryNetworkOnly
}
func (r *jobRun) serialEnabled() bool { return !r.networkOnly() && r.v.snapshot().OOB != nil }

// resetOOB sends v's Mac a Hard reset and says through which port. It
// serializes with a policy transition, which cancels it; one that waited
// behind a transition to the network-only policy is refused under it (see
// cableLocked).
func (v *dev) resetOOB(ctx context.Context) (port string, err error) {
	v.oobCallMu.Lock()
	defer v.oobCallMu.Unlock()
	v.mu.Lock()
	if v.forgotten {
		name := v.d.Name
		v.mu.Unlock()
		return "", refuse(http.StatusNotFound, "no Hard reset was sent: %s is leaving the lab", name)
	}
	op, cancel := context.WithCancel(ctx)
	v.oobCallCancel = cancel
	v.mu.Unlock()
	defer func() { cancel(); v.mu.Lock(); v.oobCallCancel = nil; v.mu.Unlock() }()
	return v.s.hardReset(op, v)
}

// This policy survives a failed return and controller restart. An empty legacy
// request inherits a network hold, and no job lifts one: the serial-assisted
// mode, which did, is refused (see errNoConsole). A Maintainer lifts it (see
// liftRecoveryHold). Nothing clears or rewrites Device.OOB.
func (s *Server) beginJobRecoveryMode(ctx context.Context, v *dev, j *api.Job) error {
	v.recoveryMu.Lock()
	defer v.recoveryMu.Unlock()
	if e := validateRecoveryTransaction(v.snapshot(), j.Spec); e != nil {
		return e
	}
	if e := s.transitionRecoveryMode(ctx, v, j.Spec.RecoveryMode, "", "", j.ID); e != nil {
		return e
	}
	mode := v.snapshot().EffectiveRecoveryMode
	// As updateJob saves the run's copy: without the people forgotten since.
	jobMu.Lock()
	defer jobMu.Unlock()
	j.EffectiveRecoveryMode = mode
	s.forgottenLocked(j)
	if err := s.store.saveJob(j); err != nil {
		return fmt.Errorf("persist effective job recovery mode: %w", err)
	}
	return nil
}
func validateRecoveryTransaction(d api.Device, spec api.JobSpec) error {
	if spec.RecoveryTransaction != "" && (spec.RecoveryTransaction != d.RecoveryTransaction || spec.Holder != d.RecoveryHolder || spec.RecoveryMode == "" || spec.RecoveryMode != d.EffectiveRecoveryMode) {
		return errors.New("job does not match selected recovery transaction")
	}
	if d.RecoveryTransaction != "" && spec.RecoveryMode != "" && spec.RecoveryTransaction == "" {
		return errors.New("explicit job mode requires selected recovery_transaction")
	}
	return nil
}
func (s *Server) transitionRecoveryMode(ctx context.Context, v *dev, requested, transaction, holder, jobID string) error {
	if requested == api.RecoverySerialAssisted {
		return errNoConsole
	}
	if !api.ValidRecoveryMode(requested) {
		return errors.New("stored job has invalid recovery_mode")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d := v.snapshot()
	mode := effectiveRecoveryMode(requested, d.EffectiveRecoveryMode)
	v.mu.Lock()
	previous := v.d.EffectiveRecoveryMode
	previousTransaction, previousHolder, previousJob := v.d.RecoveryTransaction, v.d.RecoveryHolder, v.d.RecoveryModeJob
	v.d.EffectiveRecoveryMode = mode
	if jobID != "" {
		v.d.RecoveryModeJob = jobID
	}
	if transaction != "" {
		v.d.RecoveryTransaction = transaction
		v.d.RecoveryHolder = holder
	}
	if mode == api.RecoveryNetworkOnly && v.oobCallCancel != nil {
		v.oobCallCancel()
	}
	// storeLocked, not saveDevice: once the Mac has left (or was
	// forgotten) its row is gone, and a job starting just then must not
	// write it back, where it would hold the name a new Mac enrolls under.
	if err := v.storeLocked(); err != nil {
		v.d.EffectiveRecoveryMode = previous
		v.d.RecoveryTransaction, v.d.RecoveryHolder, v.d.RecoveryModeJob = previousTransaction, previousHolder, previousJob
		v.mu.Unlock()
		return fmt.Errorf("persist recovery policy before commands: %w", err)
	}
	v.mu.Unlock()
	if mode == api.RecoveryNetworkOnly {
		drained := make(chan struct{})
		go func() { v.oobCallMu.Lock(); v.oobCallMu.Unlock(); close(drained) }()
		select {
		case <-drained:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("prior controller call did not stop; no device command issued")
		}
	}
	return nil
}

// liftRecoveryHold ends v's network-only policy and the transaction that
// selected it: the Mac takes a Hard reset again. Caller holds v.recoveryMu.
func (s *Server) liftRecoveryHold(v *dev) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	mode, transaction, holder, job := v.d.EffectiveRecoveryMode, v.d.RecoveryTransaction, v.d.RecoveryHolder, v.d.RecoveryModeJob
	v.d.EffectiveRecoveryMode, v.d.RecoveryTransaction, v.d.RecoveryHolder, v.d.RecoveryModeJob = "", "", "", ""
	if err := v.storeLocked(); err != nil {
		v.d.EffectiveRecoveryMode, v.d.RecoveryTransaction, v.d.RecoveryHolder, v.d.RecoveryModeJob = mode, transaction, holder, job
		return fmt.Errorf("persist the lifted recovery policy: %w", err)
	}
	v.publishLocked()
	return nil
}

var recoveryTransactionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)

func (s *Server) hRecoveryMode(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	if err := s.mayDo(callerOf(r), v, actRecovery); err != nil {
		httpErr(w, 403, "%v", err)
		return
	}
	var q api.RecoveryModeRequest
	d := json.NewDecoder(io.LimitReader(r.Body, 4097))
	d.DisallowUnknownFields()
	if e := d.Decode(&q); e != nil {
		httpErr(w, 400, "invalid recovery mode request")
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		httpErr(w, 400, "extra recovery mode data")
		return
	}
	if q.Mode == api.RecoverySerialAssisted {
		s.hNoConsole(w, r)
		return
	}
	lifts := q.Mode == api.RecoveryHardReset
	if q.Mode == "" || !(lifts || api.ValidRecoveryMode(q.Mode)) || q.Holder == "" || !recoveryTransactionID.MatchString(q.Transaction) {
		httpErr(w, 400, "explicit mode, holder and bounded transaction required")
		return
	}
	v.recoveryMu.Lock()
	defer v.recoveryMu.Unlock()
	current := v.snapshot()
	// The Maintainer's own Lease by account, as every fence asks; the holder
	// label is checked apart.
	if !s.holdsLease(v, leaser(r)) || current.Lease == nil || current.Lease.Holder != q.Holder || current.Lease.Expires.Sub(s.now()) < 30*time.Second || current.ActiveJob != "" || len(v.queue) > 0 {
		httpErr(w, 409, "own lease covering transition and idle device required")
		return
	}
	transition := func() error { return s.transitionRecoveryMode(r.Context(), v, q.Mode, q.Transaction, q.Holder, "") }
	if lifts {
		transition = func() error { return s.liftRecoveryHold(v) }
	}
	if e := transition(); e != nil {
		httpErr(w, 409, "recovery policy transition failed; inspect current state before retry: %v", e)
		return
	}
	writeJSON(w, api.RecoveryModeSelection{Schema: 1, Device: current.Name, Mode: q.Mode, Holder: q.Holder, Transaction: q.Transaction, Quiescent: true})
}

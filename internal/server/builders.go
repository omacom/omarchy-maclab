package server

// Builders build kernels and packages for the lab (lab-builder). The lab has
// one, on the mini, and it proves who it is with its own token.
//
// A builder is added by a Maintainer (lab builder add) and gets a token of
// its own, shown once. labd makes and accepts no other builder token: there
// is no shared one. With its token a builder may only build: take queued
// builds of the kinds it was given, and for the build it took, read its
// inputs, report progress, upload outputs and finish it. Each token is one
// generation of its name. Removing the builder ends its open requests,
// fails what it was building and drops its claims; adding the name again
// makes a new generation that inherits nothing. Claims are kept in memory,
// so a labd restart fails a running build rather than trusting a claim it
// can no longer check.
//
// A builder's kernels boot as root on lab Macs. Adding one trusts whoever runs
// it, for that, as much as a Maintainer.
//
// Who may ask for a build is at the end of this file: Maintainers, and the
// M team once their builds open.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// builderCred is the builder a request comes from: one generation of a
// named builder. The zero value is no builder: a person, on the upload
// routes people and builders share.
type builderCred struct {
	name  string
	gen   string
	kinds []string
}

func (c builderCred) nobody() bool { return c.gen == "" }

type builderCredKey struct{}

func builderCredOf(r *http.Request) builderCred {
	c, _ := r.Context().Value(builderCredKey{}).(builderCred)
	return c
}

// claim is a named builder's hold on a build it took: which generation took
// it, and what it has uploaded for it.
type claim struct {
	gen     string
	uploads map[string]bool
}

// uploadedAll says whether every output d names was uploaded for this claim.
func (cl *claim) uploadedAll(d api.BuildResult) bool {
	if cl == nil || (d.Artifact != "" && !cl.uploads[d.Artifact]) {
		return false
	}
	for _, f := range d.Files {
		if !cl.uploads[f.SHA256] {
			return false
		}
	}
	return true
}

func newBuilderToken() string { return "bldr_" + randHex(20) }

// builderReportMax caps a builder's progress or done report.
const builderReportMax = 16 << 20

// builderUploadMax caps one upload by a named builder. A kernel package is a
// few hundred MB.
var builderUploadMax int64 = 4 << 30

var errBuilderRemoved = errors.New("this builder was removed")

// builderAuth admits a builder by its own token; its request ends when it
// is removed. Nothing else opens a builder route.
func (s *Server) builderAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		b, gen, err := s.store.builderByToken(hashSecret(tok))
		if tok == "" || err != nil || gen == "" {
			s.refuse(w, r, 401, "bad builder token")
			return
		}
		c := builderCred{name: b.Name, gen: gen, kinds: b.Kinds}
		ctx, cancel := context.WithCancel(context.WithValue(r.Context(), builderCredKey{}, c))
		noteCaller(r, "builder:"+c.name)
		defer s.bconns.add(cancel, c.gen)()
		// Removed between the lookup and registering this request.
		if !s.store.builderGenCurrent(c.gen) {
			s.refuse(w, r, 401, "bad builder token")
			return
		}
		h(w, r.WithContext(ctx))
	}
}

// ownsLocked says whether c may act on build b: only while b runs on the
// claim its current generation made. Callers hold s.builds.state.
func (s *Server) ownsLocked(c builderCred, b *api.Build) error {
	if c.nobody() || !s.store.builderGenCurrent(c.gen) {
		return errBuilderRemoved
	}
	cl := s.builds.claims[b.ID]
	if cl == nil || cl.gen != c.gen || b.State != api.BuildRunning || b.Builder != c.name {
		return fmt.Errorf("build %s isn't running on builder %s", b.ID, c.name)
	}
	return nil
}

// kindsWhere is the SQL condition for the queued builds c may take, given the
// kinds its lab-builder asked for (?kinds=kernel,package; none: kernel only).
func kindsWhere(c builderCred, asked string) string {
	askedPkg := slices.Contains(splitList(asked), api.BuilderPackage)
	kernel := slices.Contains(c.kinds, api.BuilderKernel)
	pkg := askedPkg && slices.Contains(c.kinds, api.BuilderPackage)
	switch {
	case kernel && pkg:
		return `1=1`
	case kernel:
		return `json_extract(data, '$.kind') IS NULL`
	case pkg:
		return `json_extract(data, '$.kind') = '` + api.BuildPackage + `'`
	}
	return `0=1`
}

// claimBuild records that a builder polled and hands it the oldest queued
// build matching where, a Maintainer's before an M team member's, under the
// state lock: a builder runs one build at a time, two builders never take
// the same build, and a removed builder takes none and isn't seen again.
func (s *Server) claimBuild(c builderCred, host, where string) (*api.Build, error) {
	s.builds.state.Lock()
	defer s.builds.state.Unlock()
	if !s.store.builderGenCurrent(c.gen) {
		return nil, errBuilderRemoved
	}
	s.builds.mu.Lock()
	s.builds.builders[host] = s.now()
	s.builds.mu.Unlock()
	for id, cl := range s.builds.claims {
		if b, err := s.getBuild(id); cl.gen == c.gen && err == nil && b.State == api.BuildRunning {
			return nil, nil
		}
	}
	if s.buildsHeld() {
		return nil, nil
	}
	q, err := s.store.builds(`state='queued' AND (` + where + `) ORDER BY COALESCE(json_extract(data, '$.by_team'), 0) ASC, created ASC LIMIT 1`)
	if err != nil || len(q) == 0 {
		return nil, err
	}
	b := q[0]
	b.State, b.Builder, b.Started, b.Stage = api.BuildRunning, host, s.now(), "starting"
	b.ETA = s.estimate(b)
	s.builds.mu.Lock()
	s.builds.beat[b.ID] = s.now()
	s.builds.mu.Unlock()
	s.builds.claims[b.ID] = &claim{gen: c.gen, uploads: map[string]bool{}}
	s.saveBuild(b)
	return b, nil
}

func (s *Server) hBuilderSelf(w http.ResponseWriter, r *http.Request) {
	c := builderCredOf(r)
	writeJSON(w, api.BuilderSelf{Name: c.name, Kinds: c.kinds})
}

// hBuilderGetArtifact serves a builder an artifact only when it proves it
// holds a running build whose input (config or recipe) that artifact is.
func (s *Server) hBuilderGetArtifact(w http.ResponseWriter, r *http.Request) {
	c, sha := builderCredOf(r), r.PathValue("sha")
	s.builds.state.Lock()
	ok := false
	for id, cl := range s.builds.claims {
		if cl.gen != c.gen {
			continue
		}
		if b, err := s.getBuild(id); err == nil && s.ownsLocked(c, b) == nil && sha != "" &&
			(b.ConfigSHA == sha || b.Recipe == sha) {
			ok = true
			break
		}
	}
	s.builds.state.Unlock()
	if !ok {
		httpErr(w, 404, "no artifact %s for this builder now", short(sha))
		return
	}
	s.hBuilderFetch(w, r)
}

// ctxReader stops reading once ctx ends: a removed builder's upload stops
// mid-body.
type ctxReader struct {
	ctx context.Context
	io.ReadCloser
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.ReadCloser.Read(p)
}

// interruptReadsOnCancel makes a read of r's body that is waiting for the
// sender return at once when ctx ends (a builder removed mid-upload), by
// moving the connection's read deadline to now. The returned func, deferred
// by the handler, stops that before the handler returns.
func interruptReadsOnCancel(w http.ResponseWriter, ctx context.Context) (stop func()) {
	var mu sync.Mutex
	over := false
	cancel := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !over {
			readBodyBy(w, ctx, time.Now())
		}
	})
	return func() {
		cancel()
		mu.Lock()
		over = true
		mu.Unlock()
	}
}

// namedUpload stores a builder's upload for the build it names
// (?build=), capped while it streams, and records it for that build only if
// the builder still holds the build once the body is in.
func (s *Server) namedUpload(w http.ResponseWriter, r *http.Request, c builderCred) {
	id := r.URL.Query().Get("build")
	s.builds.state.Lock()
	b, err := s.getBuild(id)
	if err == nil {
		err = s.ownsLocked(c, b)
	}
	s.builds.state.Unlock()
	if err != nil {
		httpErr(w, 403, "an upload must name a build this builder is running (?build=): %v", err)
		return
	}
	defer interruptReadsOnCancel(w, r.Context())()
	sha, n, err := s.storeArtifact(http.MaxBytesReader(w, ctxReader{r.Context(), r.Body}, builderUploadMax))
	switch {
	case errors.As(err, new(*http.MaxBytesError)):
		httpErr(w, 413, "an upload in one request is limited to %d bytes", min(builderUploadMax, s.bodyMax()))
		return
	case err != nil && r.Context().Err() != nil:
		httpErr(w, 403, "%v", errBuilderRemoved)
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	if err := s.claimUpload(c, id, sha); err != nil {
		httpErr(w, 403, "not recorded: %v", err)
		return
	}
	writeJSON(w, map[string]any{"sha256": sha, "size": n})
}

// claimUpload records a stored artifact for the build a named builder still holds.
func (s *Server) claimUpload(c builderCred, id, sha string) error {
	s.builds.state.Lock()
	defer s.builds.state.Unlock()
	b, err := s.getBuild(id)
	if err == nil {
		err = s.ownsLocked(c, b)
	}
	if err != nil {
		return err
	}
	s.builds.claims[id].uploads[sha] = true
	return nil
}

// ---- Maintainer API ----

// builderKinds checks and orders the kinds a builder is given; none is kernel.
func builderKinds(kinds []string) ([]string, error) {
	if len(kinds) == 0 {
		return []string{api.BuilderKernel}, nil
	}
	var out []string
	for _, k := range []string{api.BuilderKernel, api.BuilderPackage} {
		if slices.Contains(kinds, k) {
			out = append(out, k)
		}
	}
	for _, k := range kinds {
		if k != api.BuilderKernel && k != api.BuilderPackage {
			return nil, fmt.Errorf("a builder's kinds are %s and %s, not %q", api.BuilderKernel, api.BuilderPackage, k)
		}
	}
	return out, nil
}

var reBuilderName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Server) hAddBuilder(w http.ResponseWriter, r *http.Request) {
	var req api.BuilderRequest
	if !decodeBody(w, r, 4<<10, &req) {
		return
	}
	if !reBuilderName.MatchString(req.Name) {
		httpErr(w, 400, "a builder's name is letters, digits, dots, dashes and underscores")
		return
	}
	kinds, err := builderKinds(req.Kinds)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	nb := api.NewBuilder{Builder: api.Builder{Name: req.Name, Kinds: kinds, Added: s.now(), AddedBy: maintainerName(r)},
		Token: newBuilderToken(), Generation: randHex(16)}
	if err := s.store.addBuilder(nb.Builder, nb.Generation, hashSecret(nb.Token)); err != nil {
		if s.store.hasBuilder(req.Name) {
			httpErr(w, 409, "there is already a builder named %s", req.Name)
			return
		}
		httpErr(w, 500, "%v", err)
		return
	}
	s.log.Printf("builder %s (%s) added by %s", nb.Name, joinList(kinds), nb.AddedBy)
	writeJSON(w, nb)
}

// hRemoveBuilder removes a named builder: its token stops working, its open
// requests end, and the builds it was running fail. ?generation= removes it
// only if it is still that generation (lab builder add undoing its own add),
// never one added under the same name since.
func (s *Server) hRemoveBuilder(w http.ResponseWriter, r *http.Request) {
	name, only := r.PathValue("name"), r.URL.Query().Get("generation")
	by := maintainerName(r)
	s.builds.state.Lock()
	gen, ok, err := s.store.removeBuilder(name, only)
	if err != nil || !ok {
		s.builds.state.Unlock()
		switch {
		case err != nil:
			httpErr(w, 500, "%v", err)
		case only != "" && s.store.hasBuilder(name):
			httpErr(w, 409, "builder %s is another generation now; nothing was removed", name)
		default:
			httpErr(w, 404, "no builder %s", name)
		}
		return
	}
	s.bconns.end(gen)
	failed := []string{}
	for id, cl := range s.builds.claims {
		if cl.gen != gen {
			continue
		}
		delete(s.builds.claims, id)
		b, err := s.getBuild(id)
		if err != nil || b.State != api.BuildRunning {
			continue
		}
		b.State, b.Error, b.ETA = api.BuildFailed, fmt.Sprintf("builder %s was removed by %s", name, by), 0
		s.builds.mu.Lock()
		delete(s.builds.beat, id)
		delete(s.builds.cancel, id)
		s.builds.mu.Unlock()
		s.saveBuild(b)
		failed = append(failed, id)
	}
	s.builds.state.Unlock()
	s.builds.mu.Lock()
	delete(s.builds.builders, name)
	s.builds.mu.Unlock()
	sort.Strings(failed)
	s.log.Printf("builder %s removed by %s; failed builds: %v", name, by, failed)
	writeJSON(w, api.RemovedBuilder{Removed: name, FailedBuilds: failed})
}

// hBuilders lists the builders, with the build each is running.
func (s *Server) hBuilders(w http.ResponseWriter, r *http.Request) {
	named, err := s.store.builders()
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	running, _ := s.store.builds(`state='running'`)
	on := map[string]string{}
	for _, b := range running {
		on[b.Builder] = b.ID
	}
	s.builds.mu.Lock()
	out := []api.BuilderStatus{}
	for _, b := range named {
		st := api.BuilderStatus{Host: b.Name, Registered: true, Kinds: b.Kinds, Added: b.Added, AddedBy: b.AddedBy, Build: on[b.Name]}
		if t, ok := s.builds.builders[b.Name]; ok {
			st.LastSeen, st.Alive = t, s.since(t) < time1Minute
		}
		out = append(out, st)
	}
	s.builds.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	writeJSON(w, out)
}

// ---- who may ask for a build, and who may upload ----

// Maintainers build. An M team member's build is refused until M team
// builds open (Config.MTeamBuilds), which waits for two tests of the
// Builder: that a build reaches the internet and nothing of the mini, the
// home network or the tailnet, and that one build reads nothing of another.
// Once open, a member may ask only for a build for a job on a Mac they
// lease, with one build queued or running at a time, behind the
// Maintainers' builds (claimBuild).
//
// An M team member uploads an artifact only for a job on a Mac they lease.

const (
	teamBuildsClosed = "M team builds are not open yet: only a Maintainer builds on the lab's Builder for now"
	teamBuildsNeed   = "builds are for Maintainers, and for the M team once M team builds open"
	teamUploadsNeed  = "an M team member uploads an artifact only for a job on a Mac they lease: name the Mac (--for MAC) while you hold its Lease"
)

// buildsTeam admits a Maintainer, and an M team member for the handler to
// answer by the rules above.
func (s *Server) buildsTeam(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch a, who := s.access(r); a {
		case adminAccess:
			s.asMaintainer(w, r, who, h)
		case teamAccess:
			s.asSignedIn(w, r, teamKey{}, h)
		case ownAccess:
			httpErr(w, 403, "@%s lends a Mac and sees their own Macs; %s", who, teamBuildsNeed)
		case signedInAccess:
			httpErr(w, 403, "@%s is signed in with no role in the lab; %s", who, teamBuildsNeed)
		case viewAccess:
			httpErr(w, 403, "%s may only look; %s", who, teamBuildsNeed)
		case blockedAccess:
			httpErr(w, 403, "%s", blockedSentence)
		default:
			s.noCredential(w, r)
		}
	}
}

// leasesMac is whether p holds the Lease on the Mac called name now.
func (s *Server) leasesMac(p Person, name string) bool {
	v := s.dev(name)
	return name != "" && v != nil && s.holdsLease(v, p)
}

// askedBy writes who asks for a build into req and applies the M team's
// rule. The caller runs done once the build is queued: an M team member's
// requests are answered one after another, so two at once cannot both find
// the member's queue empty.
func (s *Server) askedBy(r *http.Request, req *api.BuildRequest) (done func(), err error) {
	p := leaser(r)
	req.AskedBy, req.AskedByID, req.ByTeam = p.Handle, p.AccountID, false
	if callerOf(r).team == "" {
		return func() {}, nil
	}
	if !s.cfg.MTeamBuilds {
		return nil, refuse(403, "%s", teamBuildsClosed)
	}
	if !s.leasesMac(p, req.Device) {
		return nil, refuse(403, "an M team member builds only for a job on a Mac they lease: name the Mac (--device MAC) while you hold its Lease")
	}
	s.builds.team.Lock()
	open, err := s.store.builds(`state IN ('queued','running') AND json_extract(data, '$.asked_by_id') = ? LIMIT 1`, p.AccountID)
	if err == nil && len(open) > 0 {
		err = refuse(409, "@%s already has build %s %s: an M team member has one build queued or running at a time", p.Handle, open[0].ID, open[0].State)
	}
	if err != nil {
		s.builds.team.Unlock()
		return nil, err
	}
	req.ByTeam = true
	return s.builds.team.Unlock, nil
}

// teamUpload lets an M team member reach an upload route only for a Mac
// they lease (?device=); a Maintainer's upload passes as it is. A build's
// input (?purpose=build, as lab build and lab package send it) is refused
// with the build's own line while M team builds are closed.
func (s *Server) teamUpload(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if callerOf(r).team != "" {
			q := r.URL.Query()
			if q.Get("purpose") == "build" && !s.cfg.MTeamBuilds {
				httpErr(w, 403, "%s", teamBuildsClosed)
				return
			}
			if !s.leasesMac(leaser(r), q.Get("device")) {
				httpErr(w, 403, "%s", teamUploadsNeed)
				return
			}
		}
		h(w, r)
	}
}

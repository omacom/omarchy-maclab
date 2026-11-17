package server

// Joining a Mac with a Join command. The person who lends the Mac makes it,
// signed in, on "Lend a Mac"; a Maintainer makes one for a Lab-owned Mac
// with lab enroll (POST /api/enroll-tokens). It is one short line to run on
// the Mac: curl fetches the Join command's script from GET /join, which
// holds no secret, and bash runs it with the enrollment token in LAB_JOIN.
// The script downloads lab-agent with the token (GET /api/join/lab-agent),
// which tests the token before anything changes: a refusal is said once, by
// its case, and nothing is tried again. It checks the download's sha256
// against the one written into the script and, in a Lab release, its
// signature against the release key written into it, and runs
// `sudo lab-agent setup`, the token going to it on stdin. Setup runs its
// pre-checks, which reach no lab, then shows the lab's terms
// (GET /api/join/terms), which the owner accepts by typing yes on the
// terminal. Only then does it bind the token to the Mac
// (POST /api/join/check) and change anything; then the Mac enrolls, using
// the token up (see hEnroll).
//
// The token travels in the environment of the person's shell, then in the
// Authorization header and request bodies only, never in a URL, so neither
// labd's nor a proxy's access log records it. It reaches these three routes
// and the enrollment itself, nothing else. Every address in the line and
// the script is the Lab address.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

var reEnrollToken = regexp.MustCompile(`^enr_[0-9a-f]{32}$`)

// joinToken admits a request carrying an unexpired enrollment token, used or
// not: setup reads the terms and downloads lab-agent again when it runs
// again after an interruption.
func (s *Server) joinToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if !strings.HasPrefix(tok, "enr_") || len(tok) > 200 || !s.tokenValid(hashSecret(tok)) {
			if tok != "" {
				s.refusedToken(w, r, 401, "enrollment token unknown, used or expired")
				return
			}
			s.refuse(w, r, 401, "enrollment token unknown, used or expired")
			return
		}
		h(w, r)
	}
}

func (s *Server) hJoinTerms(w http.ResponseWriter, r *http.Request) {
	t := s.termsText()
	writeJSON(w, api.JoinTerms{SHA256: t.SHA256, Text: t.Body, Date: t.Date})
}

// hJoinCheck is the Join command testing its token on the Mac, after the
// pre-checks and the accepted terms, before anything changes. The first Mac
// to ask that the lab would take binds the token: from then on it serves the
// Mac with that activation key and no other. What would refuse the
// enrollment (Blocked, cap reached, name taken) is said here already, before
// the token is bound, so that a Mac leaves no other lab for a lab that will
// not take it.
func (s *Server) hJoinCheck(w http.ResponseWriter, r *http.Request) {
	var req api.JoinCheckRequest
	if !decodeBody(w, r, enrollBodyMax, &req) {
		return
	}
	if len(req.ActivationKey) < 32 || len(req.ActivationKey) > 128 {
		httpErr(w, 400, "activation key: want 32 to 128 characters")
		return
	}
	tokHash, keyHash := hashSecret(bearer(r)), hashSecret(req.ActivationKey)
	t, err := s.store.tokenState(tokHash, s.now())
	if err != nil {
		s.refusedToken(w, r, 401, tokenRefused)
		return
	}
	if t.BoundTo != "" && t.BoundTo != keyHash {
		s.refusedToken(w, r, 403, tokenOtherMacs)
		return
	}
	// What would refuse the enrollment is asked before the token is bound,
	// so that the same line runs again once the cap is raised or the name
	// is free: setup forgets its key on a refusal.
	if t.ActivatedBy == "" {
		if err := s.mayEnroll(t); err != nil {
			s.refusedEnrollment(w, t.Name, err)
			return
		}
	}
	if mine, err := s.store.bindToken(tokHash, keyHash); err != nil || !mine {
		s.refusedToken(w, r, 403, tokenOtherMacs)
		return
	}
	kind := api.KindLabOwned
	if t.Volunteer {
		kind = api.KindVolunteer
	}
	terms := s.termsText()
	writeJSON(w, api.JoinCheck{Name: t.Name, Kind: kind, Expires: t.Expires, Enrolled: t.ActivatedBy != "", TermsSHA256: terms.SHA256, TermsDate: terms.Date})
}

// agentSum caches the sha256 of the lab-agent labd hands out, by its size and
// modification time.
type agentSum struct {
	mu   sync.Mutex
	size int64
	mod  time.Time
	sum  string
}

var agentSums agentSum

// agentSHA256 is the sha256 of the lab-agent labd hands out, "" when it has
// none.
func (s *Server) agentSHA256() string {
	f, err := os.Open(s.cfg.AgentBinary)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	agentSums.mu.Lock()
	defer agentSums.mu.Unlock()
	if agentSums.sum != "" && agentSums.size == st.Size() && agentSums.mod.Equal(st.ModTime()) {
		return agentSums.sum
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	agentSums.size, agentSums.mod, agentSums.sum = st.Size(), st.ModTime(), hex.EncodeToString(h.Sum(nil))
	return agentSums.sum
}

// publicBase is labd's URL as a Mac reaches it: --public-url, else the
// origin r reached labd at.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != nil {
		return s.cfg.PublicURL.String()
	}
	origin, _ := s.externalOrigin(r)
	return origin
}

// reShellSafe is what the Join command's script puts in single quotes, and
// reShellBare what the Join command itself leaves unquoted: URL and token
// characters, nothing a shell gives meaning to.
var (
	reShellSafe = regexp.MustCompile(`^[A-Za-z0-9:/._~\[\]-]+$`)
	reShellBare = regexp.MustCompile(`^[A-Za-z0-9:/._-]+$`)
)

// What the Join command says, once, when the lab does not hand it the
// agent: the case, and what to do.
const (
	joinSaysNoToken    = "LAB_JOIN holds no enrollment token: copy the Join command from Lend a Mac and run it as it is."
	joinSaysUsed       = "This Join command has expired or was used: make a new one on Lend a Mac."
	joinSaysTooMany    = "Too many refused attempts from this network: wait 15 minutes, then make a new Join command."
	joinSaysChallenged = "The front door of the lab challenged this request instead of answering it: try again later or from another network."
	joinSaysOther      = "The lab did not answer as expected"
	joinSaysUnsigned   = "The lab-agent that came is not signed by the release key of Omarchy Mac Lab (or ssh-keygen is missing). Nothing was run."
)

// joinScriptText is the script labd serves at /join. Its body is one
// function, called on the last line inside braces: a download cut short
// anywhere is a definition or a group left open, which bash refuses
// whole, so it runs nothing. The token is read from LAB_JOIN and taken out
// of the environment before anything starts; it is in no process's
// arguments: printf is the shell's own, and curl and setup read it on
// stdin. setup asks its questions on the terminal (/dev/tty), as sudo asks
// for the password, so they reach the person under curl | bash too.
var joinScriptText = template.Must(template.New("join").Parse(`#!/bin/bash
# Omarchy Mac Lab's Join command, for the lab at {{.Base}}
# Run the Join command from Lend a Mac on the Mac, as yourself:
#   curl -fsSL {{.Base}}/join | LAB_JOIN=enr_... bash
# Nothing runs until the last line has come.

lab_join_this_mac() {
  local t="${LAB_JOIN-}" u='{{.Base}}' d c
  unset LAB_JOIN
  if ! [[ $t =~ ^enr_[0-9a-f]{32}$ ]]; then
    echo '{{.SaysNoToken}}' >&2
    return 1
  fi
  d="$(mktemp -d)" || return 1
  c="$(printf 'Authorization: Bearer %s\n' "$t" | curl {{.Curl}} -A lab-join -H @- -o "$d/lab-agent" -w '%{http_code}%header{cf-mitigated}' "$u/api/join/lab-agent")" || return 1
  case "$c" in
    200) ;;
    401) echo '{{.SaysUsed}}' >&2; return 1 ;;
    429) echo '{{.SaysTooMany}}' >&2; return 1 ;;
    *challenge) echo '{{.SaysChallenged}}' >&2; return 1 ;;
    *) echo "{{.SaysOther}} ($c)." >&2; return 1 ;;
  esac
  echo '{{.SHA256}}  '"$d/lab-agent" | sha256sum -c --quiet || return 1
{{- if .Signers}}
  printf 'Authorization: Bearer %s\n' "$t" | curl {{.Curl}} -f -A lab-join -H @- -o "$d/lab-agent.sig" "$u/api/join/lab-agent.sig" || return 1
  printf '%s\n' '{{.Signers}}' > "$d/signers"
  if ! ssh-keygen -Y verify -f "$d/signers" -I '{{.Signer}}' -n '{{.Namespace}}' -s "$d/lab-agent.sig" < "$d/lab-agent" >/dev/null 2>&1; then
    echo '{{.SaysUnsigned}}' >&2
    return 1
  fi
{{- end}}
  chmod +x "$d/lab-agent" || return 1
  printf '%s\n' "$t" | sudo "$d/lab-agent" setup --server "$u" --token-stdin --gui-user "$USER"{{.Plain}}
}

{ lab_join_this_mac; }
`))

// joinScript is the script labd serves at /join for r, nil when labd has no
// lab-agent to hand out. It names the current lab-agent's sha256 and, in a
// Lab release, the release key the agent's signature is checked against.
// Over https curl may use nothing else; over plain http setup must be told
// it is allowed (and only loopback is, by default). The download is the
// token's test: an answer other than the agent stops the script there,
// named by its case, before anything on the Mac changes.
func (s *Server) joinScript(r *http.Request) []byte {
	base, sum := s.publicBase(r), s.agentSHA256()
	if sum == "" || !reShellSafe.MatchString(base) {
		return nil
	}
	v := struct {
		Base, SHA256, Curl, Plain, Signers, Signer, Namespace                       string
		SaysNoToken, SaysUsed, SaysTooMany, SaysChallenged, SaysOther, SaysUnsigned string
	}{Base: base, SHA256: sum, Curl: "--proto '=https' --tlsv1.2 -sS",
		SaysNoToken: joinSaysNoToken, SaysUsed: joinSaysUsed, SaysTooMany: joinSaysTooMany, SaysChallenged: joinSaysChallenged, SaysOther: joinSaysOther, SaysUnsigned: joinSaysUnsigned}
	if !strings.HasPrefix(base, "https://") {
		v.Curl, v.Plain = "-sS", " --allow-plain-http"
	}
	// With a signature beside the agent, as in a Lab release, the script
	// checks it against the release key written in it. A labd built as a
	// Lab release never goes without; a build from a checkout has none.
	if _, ok := s.agentSig(); ok {
		key := s.releaseKey()
		if !reReleaseKey.MatchString(key) {
			return nil
		}
		v.Signers = agent.ReleaseSigner + ` namespaces="` + agent.ReleaseNamespace + `" ` + key
		v.Signer, v.Namespace = agent.ReleaseSigner, agent.ReleaseNamespace
	} else if reLabRelease.MatchString(s.cfg.Release) {
		return nil
	}
	var b bytes.Buffer
	if err := joinScriptText.Execute(&b, v); err != nil {
		return nil
	}
	return b.Bytes()
}

// hJoinScript is GET /join, the Join command's script. It holds no secret
// and asks for none, and it is made afresh for every request: never kept.
func (s *Server) hJoinScript(w http.ResponseWriter, r *http.Request) {
	script := s.joinScript(r)
	hdr := w.Header()
	hdr.Set("Cache-Control", cacheNever)
	if script == nil {
		httpErr(w, http.StatusServiceUnavailable, "This lab has no lab-agent to hand out, so it makes no Join command.")
		return
	}
	hdr.Set("Content-Type", "text/x-shellscript; charset=utf-8")
	hdr.Set("Content-Disposition", `inline; filename="join.sh"`)
	hdr.Set("Content-Length", strconv.Itoa(len(script)))
	if r.Method != http.MethodHead {
		w.Write(script)
	}
}

// joinCommand is the one line that joins a Mac with token, "" when labd
// has no Join command's script to serve. The token is in the environment
// of the person's own shell, readable by them and root only, and never in
// a URL or an argument; the script takes it from there. The leading space
// keeps the line out of the shell's history where it ignores such lines.
func (s *Server) joinCommand(r *http.Request, token string) string {
	if !reEnrollToken.MatchString(token) || s.joinScript(r) == nil {
		return ""
	}
	at := s.publicBase(r) + "/join"
	if !reShellBare.MatchString(at) {
		at = "'" + at + "'"
	}
	return " curl -fsSL " + at + " | LAB_JOIN=" + token + " bash"
}

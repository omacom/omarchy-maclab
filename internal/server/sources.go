package server

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

var reHex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// A build's source is public: labd and the Builder hold no credential for
// one. It is a repository at https://github.com/<owner>/<repo>, or at a host
// the server's configuration lists (Config.SourceHosts). An address that
// carries credentials, or could (a user, a query, a fragment that is no ref),
// is refused whatever its host. A refused source is
// never contacted: the rule is applied before anything is resolved.

var reGitHubName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}$`)

// parseSource understands what people paste: GitHub repo, branch (tree/...),
// commit and pull request URLs, or a git URL at a listed host with #ref or
// @ref. hosts are the hosts allowed beside github.com.
func parseSource(in string, hosts ...string) (api.Source, error) {
	s := api.Source{Input: strings.TrimSpace(in)}
	raw := s.Input
	if raw == "" {
		return s, fmt.Errorf("empty source")
	}
	if strings.HasPrefix(raw, "github.com/") {
		raw = "https://" + raw
	}
	if i := strings.LastIndex(raw, "#"); i > 0 {
		s.Ref = raw[i+1:]
		raw = raw[:i]
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return s, fmt.Errorf("a build's source is a public repository at https://github.com/<owner>/<repo>; this is not one")
	}
	// The address is not repeated: it may hold a secret.
	if u.User != nil {
		return s, fmt.Errorf("a build's source is public: an address that carries credentials is refused, and the lab keeps none for a source")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return s, fmt.Errorf("a build's source is public: an address with a query is refused, as a query can carry a credential")
	}
	if u.Fragment != "" || s.Ref != "" && !validRef(s.Ref) {
		return s, fmt.Errorf("a build's source is public: after # comes only a branch, tag or commit, and an address carrying anything else is refused")
	}
	host := strings.ToLower(u.Host)
	if u.Scheme != "https" {
		return s, fmt.Errorf("a build's source is fetched over https: https://github.com/<owner>/<repo>, not %s://", u.Scheme)
	}
	if host == "github.com" || host == "www.github.com" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return s, fmt.Errorf("%q is not a GitHub repository URL", in)
		}
		owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
		if !reGitHubName.MatchString(owner) || !reGitHubName.MatchString(repo) {
			return s, fmt.Errorf("%q is not a GitHub repository URL", in)
		}
		s.Repo = "https://github.com/" + owner + "/" + repo
		rest := parts[2:]
		switch {
		case len(rest) >= 2 && (rest[0] == "tree" || rest[0] == "commits"):
			s.Ref = strings.Join(rest[1:], "/") // branch names may contain slashes
		case len(rest) >= 2 && rest[0] == "commit":
			s.Ref = rest[1]
		case len(rest) >= 2 && rest[0] == "pull":
			s.Ref = "refs/pull/" + rest[1] + "/head"
		case len(rest) >= 2 && rest[0] == "releases" && rest[1] == "tag" && len(rest) >= 3:
			s.Ref = strings.Join(rest[2:], "/")
		}
		if s.Ref != "" && !validRef(s.Ref) {
			return s, fmt.Errorf("a build's source is public: its branch, tag or commit is not a ref name, and an address carrying anything else is refused")
		}
		return s, nil
	}
	if !slices.Contains(hosts, host) {
		return s, fmt.Errorf("a build's source is a public repository at https://github.com/<owner>/<repo>: %s is not on the lab's list of other hosts", host)
	}
	// A git URL at a listed host, optionally repo@ref.
	if i := strings.LastIndex(raw, "@"); i > 0 && !strings.Contains(raw[i:], "/") {
		s.Ref = raw[i+1:]
		raw = raw[:i]
		if !validRef(s.Ref) {
			return s, fmt.Errorf("a build's source is public: after @ comes only a branch, tag or commit, and an address carrying anything else is refused")
		}
	}
	s.Repo = raw
	return s, nil
}

var reRef = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+-]{0,254}$`)

// validRef is a branch, tag or commit name as git allows one, narrowed to
// characters a credential's key=value form cannot be written in.
func validRef(ref string) bool {
	return reRef.MatchString(ref) && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") &&
		!strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock")
}

// source is parseSource with the hosts this labd was configured with.
func (s *Server) source(in string) (api.Source, error) {
	return parseSource(in, s.cfg.SourceHosts...)
}

// SourceHosts checks a list for Config.SourceHosts: host names, lower case,
// with a port only where the repository is served on one.
func SourceHosts(list []string) ([]string, error) {
	var out []string
	for _, h := range list {
		h = strings.ToLower(strings.TrimSpace(h))
		if u, err := url.Parse("https://" + h); h == "" || err != nil || u.Host != h || u.User != nil {
			return nil, fmt.Errorf("source host %q: give a host name, such as git.kernel.org", h)
		}
		if h != "github.com" {
			out = append(out, h)
		}
	}
	return out, nil
}

var resolveWait = 45 * time.Second

// resolve pins the source to a commit with a single `git ls-remote`.
func resolve(ctx context.Context, s api.Source) (api.Source, error) {
	if reHex40.MatchString(s.Ref) {
		s.SHA = s.Ref
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, resolveWait)
	defer cancel()
	// A redirect is never followed: the source names the host it comes from.
	args := []string{"-c", "http.followRedirects=false", "ls-remote", "--", s.Repo}
	want := s.Ref
	if want == "" {
		want = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", append(args, want, "refs/heads/"+want, "refs/tags/"+want, "refs/tags/"+want+"^{}")...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	// git hands https to a helper of its own, which a host that stops
	// answering holds: the time limit ends git's whole process group, or the
	// helper keeps the pipes open and the call waits on it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return s, fmt.Errorf("git ls-remote %s: no answer in %s; try again", s.Repo, resolveWait)
	}
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return s, fmt.Errorf("git ls-remote %s: %s", s.Repo, msg)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			refs[f[1]] = f[0]
		}
	}
	for _, name := range []string{want, "refs/heads/" + want, "refs/tags/" + want + "^{}", "refs/tags/" + want} {
		if sha, ok := refs[name]; ok {
			s.SHA = sha
			return s, nil
		}
	}
	if regexp.MustCompile(`^[0-9a-f]{7,39}$`).MatchString(s.Ref) {
		return s, fmt.Errorf("short commit %s: give the full 40-character sha, or a branch", s.Ref)
	}
	return s, fmt.Errorf("%s has no branch or tag %q", s.Repo, want)
}

// shortRepo is owner/repo for GitHub, else the URL.
func shortRepo(repo string) string {
	return strings.TrimPrefix(repo, "https://github.com/")
}

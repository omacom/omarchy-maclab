// Command labd is the Mac test lab controller. `labd break-glass` is the
// command run on the server itself that mints a Lab token for a named
// person (breakglass.go). `labd import` carries the old site's reports into
// the lab (import.go). `labd old-lab` upgrades the old lab's data directory
// (oldlab.go). `labd stage` says whether a data directory's lab has opened
// (stage.go). `labd qr` draws the line on its input as a QR code (qr.go).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/omacom/omarchy-maclab/internal/oob"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "break-glass" {
		os.Exit(breakGlass(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "import" {
		os.Exit(runImport(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "old-lab" {
		os.Exit(runOldLab(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "stage" {
		os.Exit(stage(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "qr" {
		os.Exit(qrCode(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	home, _ := os.UserHomeDir()
	listen := flag.String("listen", envOr("MACLAB_LISTEN", "127.0.0.1:7770"), "the loopback port, for callers on this machine (the Builder, the Out-of-band controller's client side, break-glass): it believes nothing a caller forwards, and labd listens on no other network address")
	socket := flag.String("socket", "", "the tunnel's Unix socket, which cloudflared dials: made for the account labd runs as alone, and the only place the edge's CF-Connecting-IP and forwarded headers are believed (needs --public-url)")
	publicURL := flag.String("public-url", "", "the Lab address people and Macs reach labd at, e.g. https://lab.omarchy-mac.org: every link labd builds names it, and through the tunnel labd answers to no other name but --alias")
	alias := flag.String("alias", "", "the Alias, e.g. maclab.omarchy.org: the one other name the tunnel carries. A page request on it gets a 308 to the same path at the Lab address; anything under /api/ is refused")
	preview := flag.Bool("preview", false, "the lab is in Preview: answering at the Lab address, closed, before it opens. Every answer tells crawlers not to index it")
	signIn := flag.String("sign-in", "", "the folder with the lab's sign-in configuration: github-client-id and github-client-secret (the OAuth app; the secret readable by labd's account alone) and admins (the Admins' GitHub account ids, one a line). Without the app's two files sign-in is off")
	machineIDKey := flag.String("machine-id-key", "", "the file with the machine-id key, which tells one Mac's reports from another's: 64 bytes as 128 hex digits, readable by labd's account alone. Without the file labd takes no reports")
	githubURL := flag.String("github-url", "", "a stand-in for GitHub on this machine, for a lab under development, e.g. http://127.0.0.1:7791 (tools/fake-github): a loopback address only, and never beside --socket")
	data := flag.String("data", filepath.Join(home, ".local/share/maclab"), "data directory")
	ntfy := flag.String("ntfy", os.Getenv("MACLAB_NTFY"), "the notice server's address, https://ntfy.sh: each Volunteer's Mac has its own topic there, which labd keeps, and a Notice goes only to it. No topic here: a topic is a secret, and labd posts nothing else")
	notifyCmd := flag.String("notify-cmd", os.Getenv("MACLAB_NOTIFY_CMD"), `shell command run per notification with $MACLAB_TITLE/$MACLAB_BODY, e.g. 'notify-send "$MACLAB_TITLE" "$MACLAB_BODY"'`)
	bootTimeout := flag.Duration("boot-timeout", 6*time.Minute, "default time a Mac gets to come back after a reboot")
	offlineAfter := flag.Duration("offline-after", 3*time.Minute, "how long an idle Mac may be silent before labd marks it offline")
	aliveWindow := flag.Duration("alive-window", time.Minute, "how stale a heartbeat may be before a Mac counts unresponsive")
	quietGrace := flag.Duration("quiet-grace", 3*time.Minute, "how long a Mac may go silent on the same boot before a command on it counts lost")
	stageTimeout := flag.Duration("stage-timeout", 45*time.Minute, "time a Mac gets for each attempt to stage a kernel, download included")
	watchdogOpenTimeout := flag.Duration("watchdog-open-timeout", 0, "watchdog.open_timeout on every Lab boot: how long the kernel feeds the hardware watchdog before systemd must open it; an early hang resets about 30s after it (0 is half the job's boot timeout, at least 120s)")
	artifactGB := flag.Int64("artifact-budget-gb", 20, "cap on the kernel artifact store; unreferenced artifacts go first")
	var sourceHosts hostList
	flag.Var(&sourceHosts, "source-host", "a host a build's public source may come from beside github.com, such as git.kernel.org (repeatable)")
	partsAtOnce := flag.Int("parts-at-once", 1, "how many parts of an upload labd tells a client to send at the same time: 1 or 4")
	omt := flag.Bool("omt", true, "run omarchy-m-test (github.com/maralcbr/omarchy-m-testing) in every job on Macs with a desktop user, and compare it with the known-good kernel")
	omtPublish := flag.Bool("omt-publish", true, "publish by itself the Known-good runs that became a Mac's Reference: each baseline, and a scheduled refresh once a week. Off, a report is published only by hand (lab publish, --publish)")
	omtEvery := flag.Duration("omt-every", 7*24*time.Hour, "refresh each idle Mac's Reference this often: omarchy-m-test on its known-good kernel (reboots the Mac); 0 never")
	oobd := flag.String("oobd", "http://127.0.0.1:7780", "the Out-of-band controller's address, on this machine")
	oobdTokenFile := flag.String("oobd-token-file", "", "labd's copy of the Out-of-band controller's token: a file only the account labd runs as reads. With none, no Mac gets a Hard reset")
	signalSocket := flag.String("signal-socket", "", "signal-cli's daemon socket on this machine, the lab's Signal sender. With none, every line for the two groups is recorded and none is posted")
	signalDir := flag.String("signal", "", `the folder with the ids of the lab's two Signal groups, one id in each file: signal-lab-group (the Lab group, "Omarchy Mac Lab") and signal-agents-group (the owner's "Agents"). A group with no file gets nothing: its lines are recorded and not posted`)
	signalTest := flag.Bool("signal-test", false, "send one test line to each Signal group that has an id, say what became of it, and stop")
	opsSocket := flag.String("ops-socket", "", `the socket the mini's own scripts tell "Agents" a line through (scripts/mini/notify): made for the account labd runs as alone, which is its whole credential. A Deploy and a Maintenance run drain jobs through it as well`)
	opsMarks := flag.String("ops-marks", "", "the folder where the nightly backup and the Restore drill leave backup.ok and restore-drill.ok, for the alerts, the weekly digest and /status")
	opsScripts := flag.String("ops-scripts", "", "the folder of the mini's scripts: labd runs parts every minute (the tunnel, Litestream, restarts) and pending-updates for the weekly digest. With none, labd watches only what it sees by itself")
	rebootWarning := flag.Duration("reboot-warning", 5*time.Minute, "how long before a Volunteer job's first reboot its Mac shows the Volunteer a notice, which the job then waits for; 0 shows none and waits for none")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("labd takes flags, or one of the commands break-glass, import, old-lab, stage and qr first: it starts no lab given another word")
	}
	var sender server.Sender
	if *signalSocket != "" {
		sender = server.NewSignalSender(*signalSocket)
	}
	if *signalTest {
		if sender == nil {
			log.Fatal("--signal-test needs --signal-socket and --signal")
		}
		if err := server.SignalTest(context.Background(), sender, *signalDir, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *partsAtOnce != 1 && *partsAtOnce != 4 {
		log.Fatalf("--parts-at-once is 1 or 4, not %d", *partsAtOnce)
	}
	if *ntfy != "" {
		if err := server.CheckNoticeServer(*ntfy); err != nil {
			log.Fatal(err)
		}
	}
	var pub *url.URL
	if *publicURL != "" {
		u, err := server.ParsePublicURL(*publicURL)
		if err != nil {
			log.Fatal(err)
		}
		pub = u
	}
	if *alias != "" {
		a, err := server.ParseAlias(*alias)
		if err != nil {
			log.Fatal(err)
		}
		*alias = a
	}
	if *socket != "" && pub == nil {
		log.Fatal("--socket needs --public-url: through the tunnel labd answers to its Lab address")
	}
	if *githubURL != "" && *socket != "" {
		log.Fatal("--github-url is for a lab under development: a labd behind the tunnel (--socket) signs people in with GitHub itself")
	}
	app, admins, err := server.ReadSignIn(*signIn, *githubURL)
	if err != nil {
		log.Fatal(err)
	}
	idKey, err := reports.ReadMachineIDKey(*machineIDKey)
	if err != nil {
		log.Fatal(err)
	}
	controller, err := outOfBand(*oobd, *oobdTokenFile)
	if err != nil {
		log.Fatal(err)
	}
	lc := server.ListenConfig{Addr: *listen, Socket: *socket}
	if err := lc.Check(); err != nil {
		log.Fatal(err)
	}
	if *opsSocket != "" && *opsSocket == *socket {
		log.Fatal("--ops-socket and --socket are two sockets: what comes through the tunnel's is from the internet")
	}
	watch := &server.Watch{Marks: *opsMarks}
	if *opsScripts != "" {
		watch.Mini = server.MiniParts(filepath.Join(*opsScripts, "parts"))
		watch.Updates = server.PendingUpdates(filepath.Join(*opsScripts, "pending-updates"))
	}
	// Only a labd behind the tunnel asks the Alias from outside.
	if *alias != "" && *socket != "" {
		watch.Ask = server.AskOutside("labd/" + version)
	}

	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	hosts, err := server.SourceHosts(sourceHosts)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The Join command downloads the lab-agent built alongside labd.
	exe, _ := os.Executable()
	s, err := server.New(ctx, server.Config{DataDir: *data, Clock: labClock(),
		PublicURL: pub, Alias: *alias, Preview: *preview, Release: version, GitHub: app, Admins: admins, OOB: controller, MachineIDKey: idKey, Signal: sender, SignalDir: *signalDir, Watch: watch,
		OMT: *omt, OMTPublish: *omtPublish, OMTEvery: *omtEvery,
		BootTimeout: *bootTimeout, OfflineAfter: *offlineAfter, AliveWindow: *aliveWindow, QuietGrace: *quietGrace, StageTimeout: *stageTimeout, WatchdogOpenTimeout: *watchdogOpenTimeout, RebootWarning: *rebootWarning, NtfyURL: *ntfy, NotifyCmd: *notifyCmd,
		SourceHosts: hosts, ArtifactBudget: *artifactGB << 30, Transfers: server.Transfers{AtOnce: *partsAtOnce},
		ReleaseKey:  releaseKey,
		AgentBinary: filepath.Join(filepath.Dir(exe), "lab-agent-linux-arm64")}, nil)
	if err != nil {
		log.Fatal(err)
	}
	hs, err := lc.Server(s.Handler())
	if err != nil {
		log.Fatal(err)
	}
	lns, err := lc.Listen()
	if err != nil {
		log.Fatal(err)
	}
	if *opsSocket != "" {
		ln, err := server.ListenOps(*opsSocket)
		if err != nil {
			log.Fatal(err)
		}
		ops := s.OpsServer()
		go func() {
			if err := ops.Serve(ln); err != http.ErrServerClosed {
				log.Fatalf("the ops socket %s: %v", *opsSocket, err)
			}
		}()
		go func() {
			<-ctx.Done()
			ops.Close()
		}()
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	where := "http://" + lns[0].Addr().String()
	if *socket != "" {
		where += " and the tunnel's socket " + *socket
	}
	switch {
	case pub != nil && s.Preview():
		log.Printf("labd listening on %s, served as %s, in Preview, data in %s", where, pub, *data)
	case pub != nil:
		log.Printf("labd listening on %s, served as %s, data in %s", where, pub, *data)
	default:
		log.Printf("labd listening on %s, data in %s", where, *data)
	}
	switch {
	case app == nil:
		log.Printf("sign-in is off: no OAuth app (--sign-in <folder> with github-client-id and github-client-secret)")
	case *githubURL != "":
		log.Printf("sign-in goes to a stand-in for GitHub at %s, not to GitHub: this labd is under development; %d Admin(s)", *githubURL, len(admins))
	default:
		log.Printf("sign-in with GitHub is on; %d Admin(s) named in %s", len(admins), filepath.Join(*signIn, "admins"))
	}
	switch {
	case s.Preview():
		log.Printf("reports are refused: the lab is in Preview")
	case idKey == nil:
		log.Printf("reports are refused: no machine-id key (--machine-id-key <file>)")
	}
	if sender == nil {
		log.Printf("no Signal sender (--signal-socket): lines for the Lab group and \"Agents\" are recorded and not posted")
	}
	if err := server.Serve(hs, lns...); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// outOfBand is labd's side of the Out-of-band controller. A token file that
// is not there yet is no error: the mini's set-up makes it, and labd reads it
// when it sends a request. One that others can read stops labd.
func outOfBand(addr, tokenFile string) (*oob.Client, error) {
	if tokenFile == "" {
		return nil, nil
	}
	c, err := oob.NewClient(addr, tokenFile)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(tokenFile); os.IsNotExist(err) {
		log.Printf("the Out-of-band controller's token is not at %s yet: no Hard reset until the mini's set-up makes it", tokenFile)
		return c, nil
	}
	if _, err := oob.ReadToken(tokenFile); err != nil {
		return nil, err
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// hostList is a flag given once for each host.
type hostList []string

func (l *hostList) String() string { return strings.Join(*l, ",") }

func (l *hostList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

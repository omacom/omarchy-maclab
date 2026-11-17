// Command lab-agent runs on each test Mac.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/labhttp"
)

const usage = `lab-agent: the Mac side of Omarchy Mac Lab

  sudo lab-agent setup --server <Lab address> --token-stdin [--gui-user you] [--accept-terms <sha256>]
        [--autologin|--stay-awake] [--volatile-journal] [--ca-file <pem>] [--allow-plain-http]
                                  what the Join command runs on the Mac: the pre-checks, the lab's terms and a
                                  typed yes, leaving any other lab and enrolling; the weekly Availability window
                                  is set on My Macs (a Volunteer's Mac refuses --autologin and --stay-awake)
  sudo lab-agent move --server <the new Lab address> [--ca-file <pem>] [--allow-plain-http] [--no-check]
                                  for a change of the Lab address only: point this enrolled Mac at it, and change
                                  nothing else (a Mac joins another lab with that lab's Join command)
  sudo lab-agent update           take the lab's lab-agent from the lab server: its signature is checked against
                                  the release key this lab-agent pins, and one older than this one is refused
  sudo lab-agent status           what this Mac is enrolled as, its ntfy topic, its Availability window,
                                  Access level, Installed-run eligibility, latest Audit log entries, and
                                  whether the lab's changed terms wait for you on My Macs
  sudo lab-agent window show|add|remove [<days> <HH:MM-HH:MM>|always]
                                  when the lab may use a Volunteer's Mac, e.g. window add mon-fri 19:00-07:00
  sudo lab-agent pause [--until YYYY-MM-DD]
                                  end the current window now, or stay closed until that date
  sudo lab-agent open --until HH:MM
                                  open the Mac to the lab now, until HH:MM
  sudo lab-agent notify --topic URL
                                  send this Mac's notices to your own ntfy topic
  sudo lab-agent access qualification|full
                                  what Maintainers may do: Qualification (the default) allows no lab exec,
                                  --script or --gui-script; lowering it stops what it no longer allows
  sudo lab-agent publish on|off   whether this Mac's omarchy-m-test reports may be published (off until
                                  you turn it on); on lists the deletion links of those published
  sudo lab-agent log              every Maintainer action on this Mac, from its own Audit log (works offline)
  sudo lab-agent jobs             the lab's jobs on this Mac
  sudo lab-agent leave            leave the lab now (asks first; --yes skips the question): stops any job, returns
                                  to your own kernel, removes the lab's boot entries, units and device secret,
                                  tells labd, and lists what is left to remove by hand
  sudo lab-agent uninstall [--dry-run] [--force]   tell labd the Mac is leaving, then undo everything setup did
  sudo lab-agent preflight        what makes this Mac unsafe to test on
  sudo lab-agent known-good       the kernel setup would record as known-good (read-only)
  lab-agent run                   the daemon (started by lab-agent.service)
  lab-agent lab-boot-session      on a Lab boot of a Volunteer's Mac, log the GUI user in (before SDDM)
  lab-agent lab-boot-lock         lock that session at once
  lab-agent lab-boot-deadlines    reboot a Lab boot that misses its deadline (started early in boot by
                                  maclab-lab-boot-deadlines.service)
  lab-agent sim --server .. --token ..   a simulated Mac, to try the lab without hardware
  lab-agent version               its own number, the Lab release it came from and the release key it trusts
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signalContext()
	defer stop()
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var err error
	switch os.Args[1] {
	case "setup":
		server := fs.String("server", "", "the Lab address")
		token := fs.String("token", "", "the Join command's one-time enrollment token (shows in the process list and sudo's journal: prefer --token-stdin)")
		tokenStdin := fs.Bool("token-stdin", false, "read the one-time enrollment token from the first line of stdin")
		acceptTerms := fs.String("accept-terms", "", "accept the lab's terms without a terminal: the sha256 of the terms you read (<Lab address>/terms)")
		caFile := fs.String("ca-file", "", "PEM file of CA certificates for labd's https certificate, when the system doesn't trust it (kept in agent.json)")
		plain := fs.Bool("allow-plain-http", false, "allow an http:// labd on another machine: the device secret then crosses the network unencrypted (recorded in agent.json)")
		gui := fs.String("gui-user", "", "user whose Hyprland session GUI tests drive")
		auto := fs.Bool("autologin", false, "log the GUI user into the desktop on boot and turn idle lock off")
		stay := fs.Bool("stay-awake", false, "turn the GUI user's idle lock and screensaver off")
		volatile := fs.Bool("volatile-journal", false, "leave journald alone: a failed lab boot's journal is lost")
		fs.Parse(os.Args[2:])
		if *tokenStdin {
			if *token != "" {
				err = fmt.Errorf("--token-stdin goes without --token")
				break
			}
			if *token, err = agent.ReadToken(os.Stdin); err != nil {
				break
			}
		}
		err = agent.Setup(ctx, agent.SetupOptions{Server: *server, Token: *token, GUIUser: *gui, Autologin: *auto, StayAwake: *stay, VolatileJournal: *volatile,
			CAFile: *caFile, AllowPlainHTTP: *plain, AcceptTerms: *acceptTerms,
			Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }})
	case "move":
		server := fs.String("server", "", "the new Lab address")
		caFile := fs.String("ca-file", "", "PEM file of CA certificates for the new labd's https certificate, when the system doesn't trust it")
		plain := fs.Bool("allow-plain-http", false, "allow an http:// labd on another machine: the device secret then crosses the network unencrypted")
		noCheck := fs.Bool("no-check", false, "don't ask the new labd whether it knows this Mac (a move staged before it answers)")
		fs.Parse(os.Args[2:])
		if *server == "" {
			err = fmt.Errorf("usage: sudo lab-agent move --server <the new Lab address> [--ca-file <pem>] [--allow-plain-http] [--no-check]")
			break
		}
		if os.Geteuid() != 0 {
			err = fmt.Errorf("move must run as root")
			break
		}
		err = agent.Move(ctx, agent.MoveOptions{Server: *server, CAFile: *caFile, AllowPlainHTTP: *plain, NoCheck: *noCheck,
			Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }})
	case "uninstall":
		dry := fs.Bool("dry-run", false, "print each action without taking it")
		force := fs.Bool("force", false, "uninstall even with a job in flight, or with the bootloader not found")
		fs.Parse(os.Args[2:])
		err = agent.Uninstall(ctx, agent.UninstallOptions{DryRun: *dry, Force: *force,
			Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }})
	case "preflight":
		l := &agent.Linux{WorkDir: "/var/lib/maclab"}
		p, w := l.Preflight()
		if len(p) == 0 {
			fmt.Println("preflight OK")
		} else {
			fmt.Println("preflight problems:\n  - " + strings.Join(p, "\n  - "))
		}
		if len(w) > 0 {
			fmt.Println("preflight warnings:\n  - " + strings.Join(w, "\n  - "))
		}
		if len(p) > 0 {
			os.Exit(1)
		}
	case "known-good":
		var out string
		if out, err = agent.DescribeKnownGood(); err == nil {
			fmt.Println(out)
		}
	case "run":
		cfgPath := fs.String("config", agent.ConfigPath, "")
		fs.Parse(os.Args[2:])
		var cfg *agent.Config
		if agent.HasLeft("/var/lib/maclab") {
			// This Mac left the lab; all that remains is to tell labd.
			err = agent.Farewell(ctx, "/var/lib/maclab", &agent.Linux{WorkDir: "/var/lib/maclab"}, log.Printf)
		} else if cfg, err = agent.LoadConfig(*cfgPath); err == nil {
			sys := &agent.Linux{WorkDir: "/var/lib/maclab", GUIUser: cfg.GUIUser, Server: cfg.Server}
			err = agent.New(cfg, sys, nil).Run(ctx)
		}
	case "lab-boot-deadlines":
		err = agent.EnforceLabBootDeadlines(ctx, "/var/lib/maclab")
	case "lab-boot-session", "lab-boot-lock":
		var cfg *agent.Config
		if cfg, err = agent.LoadConfig(agent.ConfigPath); err == nil {
			say := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
			if os.Args[1] == "lab-boot-session" {
				err = agent.WriteLabBootAutologin(cfg, say)
			} else {
				err = agent.LockLabBootSession(cfg, say)
			}
		}
	case "sim":
		server := fs.String("server", "", "labd URL")
		token := fs.String("token", "", "enrollment token")
		delay := fs.Duration("reboot-delay", 100*time.Millisecond, "how long a simulated reboot takes")
		model := fs.String("model", "Simulated Mac", "device-tree model string to report")
		caFile := fs.String("ca-file", "", "PEM file of CA certificates for labd's https certificate")
		plain := fs.Bool("allow-plain-http", false, "allow an http:// labd on another machine")
		fs.Parse(os.Args[2:])
		var caPEM []byte
		if *caFile != "" {
			if caPEM, err = labhttp.ReadCAFile(*caFile); err != nil {
				log.Fatal(err)
			}
		}
		dir, e := os.MkdirTemp("", "lab-agent-sim-")
		if e != nil {
			log.Fatal(e)
		}
		defer os.RemoveAll(dir)
		sim := agent.NewSim(dir)
		sim.RebootDelay = *delay
		sim.Model = *model
		terms, e := agent.FetchTerms(ctx, *server, *token, labhttp.Policy{CAPEM: caPEM, AllowPlainHTTP: *plain})
		if e != nil {
			log.Fatal(e)
		}
		// A simulated Mac joins as a real one does, pre-checks and all, but
		// has no owner to read the terms: it accepts them.
		if e := agent.Setup(ctx, agent.SetupOptions{Server: *server, Token: *token, CAFile: *caFile, AllowPlainHTTP: *plain, AcceptTerms: terms.SHA256, Mac: sim,
			Log: func(f string, a ...any) { log.Printf(f, a...) }}); e != nil {
			log.Fatal(e)
		}
		var cfg *agent.Config
		if cfg, err = agent.LoadConfig(filepath.Join(dir, "agent.json")); err != nil {
			break
		}
		// A simulated Mac never touches the lab-agent installed on the
		// machine it runs on: its own is a file in its folder.
		cfg.Self = &agent.Self{Path: filepath.Join(dir, "lab-agent"), Restart: func() error { return nil }}
		log.Printf("simulated Mac enrolled as %s", cfg.Name)
		err = agent.New(cfg, sim, nil).Run(ctx)
	case "version":
		fmt.Println(agent.Version)
		fmt.Println("Lab release", agent.LabRelease)
		if agent.ReleaseKey != "" {
			fmt.Println("Release key", agent.ReleaseKey)
		}
	default:
		if agent.IsVolunteerCommand(os.Args[1]) {
			var cfg *agent.Config
			cfg, err = agent.LoadConfig(agent.ConfigPath)
			if err != nil && os.Args[1] == "log" && agent.HasLeft("/var/lib/maclab") {
				cfg, err = &agent.Config{}, nil // a Mac that left still reads its own Audit log
			}
			if err == nil {
				args := os.Args[1:]
				if args[0] == "leave" && len(args) == 1 && confirmLeaving() {
					args = append(args, "--yes")
				}
				sys := &agent.Linux{WorkDir: "/var/lib/maclab", GUIUser: cfg.GUIUser}
				err = agent.Volunteer(ctx, cfg, sys, args, os.Stdout)
			}
			break
		}
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "lab-agent:", err)
		os.Exit(1)
	}
}

// signalContext ends when lab-agent is told to stop: Ctrl-C, systemd, or
// the terminal going away (SIGHUP, as when the SSH session that runs setup
// drops). Each ends ctx instead of killing the process, so a command stops
// where it can be run again.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

// confirmLeaving asks the Volunteer, on their terminal, whether to leave the
// lab. Without a terminal it says no, and leave asks for --yes.
func confirmLeaving() bool {
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	fmt.Print("Leave the lab now? Any job stops, your Mac returns to its own kernel, and the lab is removed from it. [y/N] ")
	var answer string
	fmt.Scanln(&answer)
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
}

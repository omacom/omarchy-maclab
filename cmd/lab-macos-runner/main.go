// Command lab-macos-runner runs the lab's macOS benchmarks on a lab Mac, in
// macOS: a LaunchDaemon starts it at every boot (see release/macos-runner).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/omacom/omarchy-maclab/internal/macosrunner"
)

const usage = `lab-macos-runner: the macOS side of Omarchy Mac Lab's macOS runs

  sudo lab-macos-runner enroll --server <Lab address> --token-stdin [--gui-user you]
                          enroll this Mac's runner once, with the line lab macos enroll <mac>
                          or Set up macOS on My Macs gives
  sudo lab-macos-runner setup
                          asks, at the Mac, whether to turn FileVault off and whether to log you
                          in automatically, so that a macOS run needs nobody; changes nothing
                          without your yes
  lab-macos-runner boot   what the LaunchDaemon runs at boot: tells the lab whether this boot
                          reached the desktop with nobody typing; with a macOS run queued for this
                          Mac, runs it, uploads the results and reboots back to Omarchy
  lab-macos-runner version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	config := fs.String("config", macosrunner.ConfigPath, "the runner's enrolment")
	var err error
	switch os.Args[1] {
	case "enroll":
		server := fs.String("server", "", "the Lab address")
		tokenStdin := fs.Bool("token-stdin", false, "read the one-time token from the first line of stdin")
		gui := fs.String("gui-user", os.Getenv("SUDO_USER"), "whose desktop session the browsers run in")
		fs.Parse(os.Args[2:])
		if *server == "" || !*tokenStdin {
			err = fmt.Errorf("usage: sudo lab-macos-runner enroll --server <Lab address> --token-stdin [--gui-user you]")
			break
		}
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if err = macosrunner.Enroll(ctx, *server, strings.TrimSpace(line), *gui, *config); err == nil {
			fmt.Printf("this Mac's macOS runner is enrolled; it runs at every boot (%s)\n", *config)
			fmt.Println("Next, so that a macOS run needs nobody here: sudo lab-macos-runner setup")
		}
	case "setup":
		fs.Parse(os.Args[2:])
		if os.Geteuid() != 0 {
			err = fmt.Errorf("run it with sudo: sudo lab-macos-runner setup")
			break
		}
		var tty *os.File
		if tty, err = os.OpenFile("/dev/tty", os.O_RDWR, 0); err != nil {
			err = fmt.Errorf("setup asks its questions on the terminal, and there is none: %w", err)
			break
		}
		defer tty.Close()
		s := &macosrunner.Setup{In: tty, Out: tty, User: os.Getenv("SUDO_USER")}
		err = s.Run(ctx)
	case "boot":
		fs.Parse(os.Args[2:])
		var cfg macosrunner.Config
		if cfg, err = macosrunner.LoadConfig(*config); err == nil {
			r := &macosrunner.Runner{Config: cfg, Dir: macosrunner.WorkDir, Log: log.Printf}
			if _, aerr := r.Attendance(ctx, macosrunner.MacSeat(), macosrunner.AttendanceWait); aerr != nil {
				log.Printf("telling the lab whether this Mac is Attended: %v", aerr)
			}
			err = r.Boot(ctx)
		}
	case "version":
		fmt.Println(macosrunner.Version)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

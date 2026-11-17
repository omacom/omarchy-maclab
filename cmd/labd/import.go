package main

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/omacom/omarchy-maclab/internal/oldsite"
	"github.com/omacom/omarchy-maclab/internal/oldsite/pg"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/server"
)

// `labd import` carries the old site's reports into the lab, on the server
// itself (spec section 5, decision S9):
//
//	labd import dump --to FILE     reads the old site's Postgres into FILE
//	labd import load --from FILE   fills an empty reports.db from FILE
//	labd import check counts --from FILE
//	labd import check machine-id --machine-id-key FILE --public-key FILE
//	labd import check deletion-link
//	labd import check certificate [--save FILE]
//
// The address of the old site's Postgres holds its password. dump asks for
// it at a prompt that does not show it; it is taken from no argument, no
// variable of the environment and no file, and is written to none and to
// no output. load reads the dump file and never the old site.

const importUsage = `usage:
  labd import dump --to FILE
      Reads the old site's three tables from its Postgres into FILE, a new
      file only this account reads. It asks for the Postgres address at a
      prompt and does not show it: the address is never an argument. Then
      it asks for the CA file the Postgres certificate must chain to
      (sslmode verify-ca); beyond this machine nothing is read without it.
  labd import load --from FILE [--data DIR]
      Fills an empty reports.db from FILE, and makes the people it names.
      It reads FILE only. On a reports.db that holds anything it refuses.
  labd import check counts --from FILE [--data DIR]
      The row counts of reports.db against FILE's.
  labd import check machine-id --machine-id-key FILE --public-key FILE [--data DIR]
      One Mac's machine id, recomputed from its Machine public key, is on
      what the import carried.
  labd import check deletion-link [--data DIR]
      A deletion link the old site issued, asked for at a prompt, deletes
      its report.
  labd import check certificate [--save FILE]
      The certificate chain the old site's Postgres presents, each with its
      SHA-256 fingerprint, to look at before its CA file is trusted. It asks
      for the address at a prompt and sends neither user nor password. With
      --save, the chain's last certificate is written to FILE, a new file,
      for the dump's CA file.
`

// oldSiteDB is the old site's Postgres as the dump reads it.
type oldSiteDB interface {
	oldsite.Source
	Close() error
}

// openOldSite opens the old site's Postgres at the address typed, its
// certificate checked against the CA file typed.
var openOldSite = pgOpen

// oldSiteChain is the certificate chain the old site's Postgres presents.
var oldSiteChain = pg.Chain

func pgOpen(address, caFile string) (oldSiteDB, error) {
	db, err := pg.Open(address, caFile)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// asked reads one line the person types at prompt, without showing it. With
// no terminal on in it reads the line as it comes, for a caller that hands
// it over a pipe.
func asked(in io.Reader, errw io.Writer, prompt string) (string, error) {
	var line string
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(errw, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(errw)
		if err != nil {
			return "", errors.New("nothing could be read at the prompt")
		}
		line = string(b)
	} else {
		// A byte at a time, so that the next prompt reads the next line.
		var b strings.Builder
		one := make([]byte, 1)
		for b.Len() < 8192 {
			n, err := in.Read(one)
			if n == 1 {
				if one[0] == '\n' {
					break
				}
				b.WriteByte(one[0])
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", errors.New("nothing could be read at the prompt")
			}
		}
		line = b.String()
	}
	return strings.TrimSpace(line), nil
}

func importCommand(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) int {
	usage := func() int {
		fmt.Fprint(errw, importUsage)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("labd import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	data := fs.String("data", filepath.Join(home, ".local/share/maclab"), "")
	to := fs.String("to", "", "")
	from := fs.String("from", "", "")
	githubURL := fs.String("github-url", "", "")
	idKey := fs.String("machine-id-key", "", "")
	publicKey := fs.String("public-key", "", "")
	save := fs.String("save", "", "")
	what, rest := args[0], args[1:]
	if what == "check" && len(rest) > 0 {
		what, rest = "check "+rest[0], rest[1:]
	}
	// What follows is never repeated: an address typed as an argument by
	// mistake must not reach a log.
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(errw, "labd import: an argument it does not take. Nothing was done.")
		return usage()
	}
	fail := func(err error) int {
		fmt.Fprintln(errw, "labd import:", err)
		return 1
	}
	var err error
	switch what {
	case "dump":
		if *to == "" {
			return usage()
		}
		err = importDump(ctx, *to, in, out, errw)
	case "load":
		if *from == "" {
			return usage()
		}
		err = importLoad(ctx, *data, *from, *githubURL, out)
	case "check counts":
		if *from == "" {
			return usage()
		}
		err = checkCounts(ctx, *data, *from, out)
	case "check machine-id":
		if *idKey == "" || *publicKey == "" {
			return usage()
		}
		err = checkMachineID(*data, *idKey, *publicKey, out)
	case "check deletion-link":
		err = checkDeletionLink(*data, in, out, errw)
	case "check certificate":
		err = checkCertificate(ctx, *save, in, out, errw)
	default:
		return usage()
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func importDump(ctx context.Context, to string, in io.Reader, out, errw io.Writer) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s is already there: a dump is never replaced. Name another file, or remove that one", to)
	}
	address, err := asked(in, errw, "Address of the old site's Postgres (it is not shown): ")
	if err != nil {
		return err
	}
	if address == "" {
		return errors.New("no address was given. Nothing was read")
	}
	caFile, err := asked(in, errw, "CA file the old site's Postgres certificate chains to (empty only for a Postgres on this machine): ")
	if err != nil {
		return err
	}
	db, err := openOldSite(address, caFile)
	address = ""
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	counts, err := oldsite.WriteDump(ctx, to, db, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s: %v, as they stood at one moment. The old site was only read.\n", to, counts)
	return nil
}

// dataFile is a database of labd's data directory, which must be there and
// belong to the account that runs the command.
func dataFile(data, name string) (string, error) {
	path := filepath.Join(data, name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no %s in %s: name labd's data directory with --data", name, data)
	}
	if err := asDataOwner(path); err != nil {
		return "", errors.New(strings.ReplaceAll(err.Error(), "break-glass", "import"))
	}
	return path, nil
}

func openReports(data string) (*reports.Store, error) {
	path, err := dataFile(data, "reports.db")
	if err != nil {
		return nil, err
	}
	return reports.Open(path)
}

func importLoad(ctx context.Context, data, from, githubURL string, out io.Writer) error {
	dump, err := oldsite.OpenDump(from)
	if err != nil {
		return err
	}
	if _, err := dataFile(data, "labd.db"); err != nil {
		return err
	}
	store, err := openReports(data)
	if err != nil {
		return err
	}
	defer store.Close()
	if held, _, signIns, digests, err := store.Holds(); err != nil {
		return err
	} else if held+signIns+digests > 0 {
		return &reports.NotEmpty{Reports: held, SignIns: signIns, Digests: digests}
	}
	lookup, err := server.GitHubLookup(githubURL)
	if err != nil {
		return err
	}
	people, notes, err := reports.CarriedPeople(ctx, dump, lookup)
	if err != nil {
		return fmt.Errorf("%w. Nothing was imported: start it again", err)
	}
	// People first: they are added to, never replaced, so a run cut here
	// is started again and finds them there.
	added, err := server.CarryPeople(data, people, time.Now())
	if err != nil {
		return err
	}
	carried, err := store.Carry(ctx, dump)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Imported %s (taken %s): %d reports (%d hidden, %d Verified runs), %d Mac sign-ins, %d digests; %d people, %d of them new to the lab.\n",
		from, dump.TakenAt.Format(time.RFC3339), carried.Reports, carried.Hidden, carried.Verified, carried.SignIns, carried.Digests, len(people), added)
	if len(carried.Listed)+len(notes) == 0 {
		fmt.Fprintln(out, "Every report passes the lab's rules.")
		return nil
	}
	fmt.Fprintf(out, "Listed, %d (all were carried):\n", len(carried.Listed)+len(notes))
	for _, l := range carried.Listed {
		fmt.Fprintf(out, "  report %s: %s\n", l.PublicID, l.Reason)
	}
	for _, note := range notes {
		fmt.Fprintf(out, "  %s\n", note)
	}
	return nil
}

func checkCounts(ctx context.Context, data, from string, out io.Writer) error {
	dump, err := oldsite.OpenDump(from)
	if err != nil {
		return err
	}
	var hidden int
	var accounts []int64
	unpinned := 0
	if _, err := dump.Read(ctx, oldsite.Funcs{
		OnTester: func(row *oldsite.Tester) error {
			if row.GitHubID != nil {
				accounts = append(accounts, *row.GitHubID)
			} else {
				unpinned++
			}
			return nil
		},
		OnTesterBinding: func(row *oldsite.TesterBinding) error { accounts = append(accounts, row.GitHubID); return nil },
		OnReport: func(row *oldsite.Report) error {
			if row.HiddenAt != nil {
				hidden++
			}
			return nil
		},
	}); err != nil {
		return err
	}
	store, err := openReports(data)
	if err != nil {
		return err
	}
	defer store.Close()
	held, heldHidden, signIns, _, err := store.Holds()
	if err != nil {
		return err
	}
	if _, err := dataFile(data, "labd.db"); err != nil {
		return err
	}
	distinct := map[int64]bool{}
	for _, id := range accounts {
		distinct[id] = true
	}
	known, err := server.KnownAccounts(data, accounts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "reports: %d at the old site, %d in the dump, %d in reports.db\n", dump.Source.Reports, dump.Rows.Reports, held)
	fmt.Fprintf(out, "hidden reports: %d in the dump, %d in reports.db\n", hidden, heldHidden)
	fmt.Fprintf(out, "tester_bindings: %d at the old site, %d in the dump, %d Mac sign-ins in reports.db\n", dump.Source.TesterBindings, dump.Rows.TesterBindings, signIns)
	fmt.Fprintf(out, "testers: %d at the old site, %d in the dump; the %d accounts they and the sign-ins name: %d are people. %d handles never signed in there: each is a person if GitHub knows it\n",
		dump.Source.Testers, dump.Rows.Testers, len(distinct), known, unpinned)
	if int64(held) != dump.Rows.Reports || hidden != heldHidden || int64(signIns) != dump.Rows.TesterBindings || known != len(distinct) {
		return errors.New("the row counts do not match")
	}
	fmt.Fprintln(out, "The row counts match.")
	return nil
}

func checkMachineID(data, keyFile, publicKeyFile string, out io.Writer) error {
	key, err := reports.ReadMachineIDKey(keyFile)
	if err != nil {
		return err
	}
	if key == nil {
		return fmt.Errorf("no machine-id key at %s", keyFile)
	}
	rules, err := reports.NewRules(key)
	if err != nil {
		return err
	}
	publicKey, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return err
	}
	id, err := rules.MachineIDOf(string(publicKey))
	if err != nil {
		return fmt.Errorf("%s: %w", publicKeyFile, err)
	}
	store, err := openReports(data)
	if err != nil {
		return err
	}
	defer store.Close()
	held, signIns, err := store.OfMachine(id)
	if err != nil {
		return err
	}
	if held+signIns == 0 {
		return errors.New("that Mac's machine id, recomputed from its public key, is on no report and no Mac sign-in: the machine-id key is not the old site's, or the public key is not that Mac's")
	}
	fmt.Fprintf(out, "That Mac's machine id, recomputed from its public key, is on %d reports and %d Mac sign-ins.\n", held, signIns)
	return nil
}

func checkDeletionLink(data string, in io.Reader, out, errw io.Writer) error {
	store, err := openReports(data)
	if err != nil {
		return err
	}
	defer store.Close()
	link, err := asked(in, errw, "A deletion link the old site issued (it is not shown): ")
	if err != nil {
		return err
	}
	if err := store.DeleteByOldLink(link); err != nil {
		return err
	}
	fmt.Fprintln(out, "The old site's link deleted its report on the lab, and the report's digest is kept.")
	return nil
}

func runImport(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return importCommand(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

// fingerprint is a certificate's SHA-256, as a browser shows it.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// checkCertificate shows the chain the old site's Postgres presents, and
// with save keeps its last certificate as the CA file for the dump.
func checkCertificate(ctx context.Context, save string, in io.Reader, out, errw io.Writer) error {
	if save != "" {
		if _, err := os.Lstat(save); err == nil {
			return fmt.Errorf("%s is already there: it is never replaced. Name another file, or remove that one", save)
		}
	}
	address, err := asked(in, errw, "Address of the old site's Postgres (it is not shown): ")
	if err != nil {
		return err
	}
	if address == "" {
		return errors.New("no address was given. Nothing was asked")
	}
	certs, err := oldSiteChain(ctx, address)
	address = ""
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "The old site's Postgres presents %d certificates, its own first:\n", len(certs))
	for i, cert := range certs {
		fmt.Fprintf(out, "%d. subject %s\n   issuer %s\n   valid %s to %s\n   SHA-256 %s\n", i+1, cert.Subject, cert.Issuer,
			cert.NotBefore.UTC().Format(time.DateOnly), cert.NotAfter.UTC().Format(time.DateOnly), fingerprint(cert.Raw))
	}
	if save == "" {
		return nil
	}
	last := certs[len(certs)-1]
	f, err := os.OpenFile(save, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("%s could not be made: %v", save, err)
	}
	if err := errors.Join(pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: last.Raw}), f.Close()); err != nil {
		os.Remove(save)
		return fmt.Errorf("%s could not be written: %v", save, err)
	}
	fmt.Fprintf(out, "Wrote %s: certificate %d, the chain's last. Give it at the dump's CA file prompt only once its fingerprint is the one you checked.\n", save, len(certs))
	return nil
}

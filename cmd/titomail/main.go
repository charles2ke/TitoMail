// Command titomail runs the TitoMail mail server and provides
// administration commands.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/charles2ke/TitoMail/internal/api"
	"github.com/charles2ke/TitoMail/internal/config"
	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/server"
	"github.com/charles2ke/TitoMail/internal/store"
	"github.com/charles2ke/TitoMail/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usageText = `TitoMail — a self-hosted email server with webmail.

Usage:
  titomail [serve] [flags]          run the server (see "titomail serve -h")
  titomail user add <email>         create an account (--admin, --name, --quota-mb)
  titomail user passwd <email>      set an account's password
  titomail user list                list accounts
  titomail domain add <domain>      add a mail domain and print its DNS records
  titomail domain list              list mail domains
  titomail dns <domain>             print the DNS records a domain needs
  titomail version                  print the version

Administration commands accept --data-dir (env TITOMAIL_DATA_DIR) and read
passwords from TITOMAIL_PASSWORD, standard input or an interactive prompt.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "titomail:", err)
			os.Exit(1)
		}
	}
}

func run(args []string, out io.Writer) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	sub := ""
	if (cmd == "user" || cmd == "domain") && len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch cmd + " " + sub {
	case "serve ":
		return serve(args)
	case "user add":
		return userAdd(args, out)
	case "user passwd":
		return userPasswd(args, out)
	case "user list":
		return userList(args, out)
	case "domain add":
		return domainAdd(args, out)
	case "domain list":
		return domainList(args, out)
	case "dns ":
		return dnsCmd(args, out)
	case "version ":
		fmt.Fprintln(out, version)
		return nil
	case "help ":
		fmt.Fprint(out, usageText)
		return nil
	}
	fmt.Fprint(os.Stderr, usageText)
	return fmt.Errorf("unknown command %q", strings.TrimSpace(cmd+" "+sub))
}

func serve(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("invalid log-level %q", cfg.LogLevel)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, server.Options{Config: cfg, Static: web.Dist(), Version: version, Logger: logger})
}

// adminFlags returns a flag set for an administration command. The
// configuration is loaded from the environment; only the options relevant
// to administration are exposed as flags.
func adminFlags(name string) (*flag.FlagSet, *config.Config) {
	cfg := config.Default()
	config.Register(flag.NewFlagSet("", flag.ContinueOnError), &cfg)
	fs := flag.NewFlagSet("titomail "+name, flag.ContinueOnError)
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory (env TITOMAIL_DATA_DIR)")
	return fs, &cfg
}

// parse parses flags that may appear before or after positional arguments
// and checks the number of positional arguments.
func parse(fs *flag.FlagSet, args []string, want int, usage string) ([]string, error) {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s %s\n", fs.Name(), usage)
		fs.PrintDefaults()
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) != want {
		fs.Usage()
		return nil, fmt.Errorf("%s: expected %d argument(s)", fs.Name(), want)
	}
	return pos, nil
}

func openStore(cfg *config.Config) (*store.Store, error) {
	if _, err := os.Stat(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("data directory %s: %w", cfg.DataDir, err)
	}
	return server.OpenStore(cfg.DataDir)
}

func readPassword() (string, error) {
	if pw := os.Getenv("TITOMAIL_PASSWORD"); pw != "" {
		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password: ")
		a, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", errors.New("no password given (set TITOMAIL_PASSWORD or pass it on standard input)")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func userAdd(args []string, out io.Writer) error {
	fs, cfg := adminFlags("user add")
	admin := fs.Bool("admin", false, "grant administrator access")
	name := fs.String("name", "", "display name")
	quota := fs.Int64("quota-mb", 0, "storage quota in MiB (0 = unlimited)")
	pos, err := parse(fs, args, 1, "[flags] <email>")
	if err != nil {
		return err
	}
	addr := email.NormalizeAddress(pos[0])
	if !email.ValidAddress(addr) {
		return fmt.Errorf("invalid address %q", pos[0])
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	if !st.IsLocalDomain(ctx, email.DomainOf(addr)) {
		return fmt.Errorf("%s is not a local domain; add it first with: titomail domain add %s", email.DomainOf(addr), email.DomainOf(addr))
	}
	pw, err := readPassword()
	if err != nil {
		return err
	}
	u, err := st.CreateUser(ctx, store.NewUser{Email: addr, Password: pw, DisplayName: *name, IsAdmin: *admin, QuotaBytes: *quota << 20})
	if err != nil {
		return err
	}
	role := "user"
	if u.IsAdmin {
		role = "administrator"
	}
	fmt.Fprintf(out, "Created %s %s\n", role, u.Email)
	return nil
}

func userPasswd(args []string, out io.Writer) error {
	fs, cfg := adminFlags("user passwd")
	pos, err := parse(fs, args, 1, "[flags] <email>")
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.UserByEmail(ctx, pos[0])
	if err != nil {
		return fmt.Errorf("%s: %w", pos[0], err)
	}
	pw, err := readPassword()
	if err != nil {
		return err
	}
	if err := st.SetPassword(ctx, u.ID, pw); err != nil {
		return err
	}
	if err := st.DeleteUserSessions(ctx, u.ID, ""); err != nil {
		return err
	}
	fmt.Fprintf(out, "Password changed for %s; existing web sessions were signed out\n", u.Email)
	return nil
}

func userList(args []string, out io.Writer) error {
	fs, cfg := adminFlags("user list")
	if _, err := parse(fs, args, 0, "[flags]"); err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tNAME\tROLE\tSTATUS\tUSED\tQUOTA")
	for _, u := range users {
		role, status, quota := "user", "active", "unlimited"
		if u.IsAdmin {
			role = "admin"
		}
		if u.Disabled {
			status = "disabled"
		}
		if u.QuotaBytes > 0 {
			quota = humanBytes(u.QuotaBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Email, u.DisplayName, role, status, humanBytes(u.UsedBytes), quota)
	}
	return tw.Flush()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func domainAdd(args []string, out io.Writer) error {
	fs, cfg := adminFlags("domain add")
	pos, err := parse(fs, args, 1, "[flags] <domain>")
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pos[0])), ".")
	d, err := st.CreateDomain(context.Background(), name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Added domain %s.\n\n", d.Name)
	return printDNS(out, d, cfg)
}

func domainList(args []string, out io.Writer) error {
	fs, cfg := adminFlags("domain list")
	if _, err := parse(fs, args, 0, "[flags]"); err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	domains, err := st.ListDomains(context.Background())
	if err != nil {
		return err
	}
	for _, d := range domains {
		fmt.Fprintln(out, d.Name)
	}
	return nil
}

func dnsCmd(args []string, out io.Writer) error {
	fs, cfg := adminFlags("dns")
	fs.StringVar(&cfg.Hostname, "hostname", cfg.Hostname, "public FQDN of the mail server (env TITOMAIL_HOSTNAME)")
	pos, err := parse(fs, args, 1, "[flags] <domain>")
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	d, err := st.DomainByName(context.Background(), pos[0])
	if err != nil {
		return fmt.Errorf("domain %s: %w", pos[0], err)
	}
	return printDNS(out, d, cfg)
}

func printDNS(out io.Writer, d *store.Domain, cfg *config.Config) error {
	recs, err := api.ExpectedDNS(d, cfg.Hostname, portOf(cfg.IMAPSAddr), portOf(cfg.SubmissionsAddr))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "DNS records for %s (mail server %s):\n\n", d.Name, cfg.Hostname)
	for _, r := range recs {
		req := "recommended"
		if r.Required {
			req = "required"
		}
		fmt.Fprintf(out, "%-6s %s\n       %s\n       %s, %s\n\n", r.Type, r.Name, r.Value, req, r.Purpose)
	}
	return nil
}

func portOf(addr string) int {
	if addr == "" {
		return 0
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// Command quicksurvey serves the survey application and administers its
// accounts.
//
//	quicksurvey serve
//	quicksurvey user add|list|passwd|role|rm
//	quicksurvey export -survey ID [-kind responses|summary]
//	quicksurvey healthcheck
//	quicksurvey backup -to FILE|-
//	quicksurvey initial-password
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bufio"

	// The container image is distroless: it has no zone database of its own,
	// and $QS_TZ names a zone. Carry one in the binary.
	_ "time/tzdata"

	"github.com/woodysmith1912/quicksurvey/internal/export"
	"github.com/woodysmith1912/quicksurvey/internal/store"
	"github.com/woodysmith1912/quicksurvey/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quicksurvey:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New(`usage:
  quicksurvey serve
  quicksurvey user add    -name NAME [-role viewer|editor|admin]
  quicksurvey user list
  quicksurvey user passwd -name NAME
  quicksurvey user role   -name NAME -role ROLE
  quicksurvey user rm     -name NAME
  quicksurvey export      -survey ID [-kind responses|summary]
  quicksurvey healthcheck [-url URL]
  quicksurvey backup      -to FILE|-   ("-" streams to stdout)
  quicksurvey initial-password

The data directory comes from -data or $QS_DATA_DIR (default /data).`)
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "user":
		return userCmd(args[1:])
	case "export":
		return exportCmd(args[1:])
	case "healthcheck":
		return healthcheck(args[1:])
	case "backup":
		return backupCmd(args[1:])
	case "initial-password":
		return initialPasswordCmd(args[1:])
	case "-h", "--help", "help":
		fmt.Println(usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%w", args[0], usage())
	}
}

// env returns the environment variable or a fallback.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func openStore(dir string) (*store.Store, error) {
	st, err := store.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("data directory %s: %w", dir, err)
	}
	return st, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("data", env("QS_DATA_DIR", "/data"), "data directory ($QS_DATA_DIR)")
	addr := fs.String("addr", env("QS_ADDR", ":8080"), "listen address ($QS_ADDR)")
	baseURL := fs.String("base-url", env("QS_BASE_URL", ""),
		"external origin used in shareable links, e.g. https://survey.example.com ($QS_BASE_URL)")
	tzName := fs.String("tz", env("QS_TZ", "Local"), "timezone for displaying and entering times ($QS_TZ)")
	secure := fs.Bool("secure-cookies", envBool("QS_SECURE_COOKIES", true),
		"mark cookies Secure; set false only when serving plain HTTP ($QS_SECURE_COOKIES)")
	redirect := fs.Bool("redirect-https", envBool("QS_REDIRECT_HTTPS", false),
		"redirect plain-HTTP requests to https, using X-Forwarded-Proto ($QS_REDIRECT_HTTPS)")
	trustProxy := fs.Bool("trust-proxy", envBool("QS_TRUST_PROXY", true),
		"take the client address for rate limiting from X-Real-Ip/X-Forwarded-For; "+
			"set false only when serving the internet directly ($QS_TRUST_PROXY)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	loc, err := time.LoadLocation(*tzName)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", *tzName, err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := openStore(*dir)
	if err != nil {
		return err
	}
	if err := bootstrapAdmin(st, log); err != nil {
		return err
	}

	srv, err := web.New(st, web.Config{
		BaseURL: *baseURL, SecureCookies: *secure, RedirectHTTPS: *redirect,
		TrustProxy: *trustProxy, Location: loc, Logger: log,
	})
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", *addr, "data", *dir, "tz", loc.String(),
		"secure_cookies", *secure, "redirect_https", *redirect, "trust_proxy", *trustProxy)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

// bootstrapAdmin makes a brand-new deployment usable without a manual step: if
// there are no accounts, create one and print its password once. The account
// cannot do anything until that password is changed.
func bootstrapAdmin(st *store.Store, log *slog.Logger) error {
	if st.UserCount() > 0 {
		return nil
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	pw := base64.RawURLEncoding.EncodeToString(b)
	if _, err := st.AddUserMustChange("admin", store.RoleAdmin, pw); err != nil {
		return err
	}
	log.Warn("no accounts existed, so a first administrator was created",
		"username", "admin",
		"password_file", st.InitialPasswordFile(),
		"note", "the password is in that file, not in this log; it is removed once changed")
	return nil
}

// healthcheck exists so the image can declare a HEALTHCHECK without shipping a
// shell or an HTTP client. It is the only thing in the binary that makes an
// outbound request, and only ever to the local listener.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	target := fs.String("url", "http://127.0.0.1"+env("QS_ADDR", ":8080")+"/healthz", "URL to probe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", *target, resp.Status)
	}
	return nil
}

// backupCmd writes a consistent copy of the database, safely, while the server
// is still running. Copying the file by hand is not equivalent: a live database
// has a write-ahead log beside it, and a plain copy can catch it mid-write.
func backupCmd(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dir := fs.String("data", env("QS_DATA_DIR", "/data"), "data directory ($QS_DATA_DIR)")
	to := fs.String("to", "", `file to write the backup to, or "-" for stdout`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New(`-to is required (use "-" to stream to stdout)`)
	}
	st, err := openStore(*dir)
	if err != nil {
		return err
	}
	defer st.Close()

	// Streaming is how a backup leaves a distroless container: there is no tar
	// in the image, so kubectl cp cannot work.
	//
	//	kubectl exec quicksurvey-0 -- quicksurvey backup -to - > backup.db
	if *to == "-" {
		return st.BackupToWriter(os.Stdout)
	}
	if err := st.BackupTo(*to); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *to)
	return nil
}

// initialPasswordCmd prints the bootstrap administrator's password.
//
// This exists because the image is distroless. Documenting `exec ... cat
// /data/initial-password` would be documenting a command that cannot run:
// there is no cat in the image and no shell to run one.
func initialPasswordCmd(args []string) error {
	fs := flag.NewFlagSet("initial-password", flag.ExitOnError)
	dir := fs.String("data", env("QS_DATA_DIR", "/data"), "data directory ($QS_DATA_DIR)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore(*dir)
	if err != nil {
		return err
	}
	defer st.Close()
	pw, err := st.InitialPassword()
	if err != nil {
		return err
	}
	fmt.Println(pw)
	return nil
}

func userCmd(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	fs := flag.NewFlagSet("user "+args[0], flag.ExitOnError)
	dir := fs.String("data", env("QS_DATA_DIR", "/data"), "data directory ($QS_DATA_DIR)")
	name := fs.String("name", "", "username")
	role := fs.String("role", string(store.RoleEditor), "viewer, editor or admin")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	st, err := openStore(*dir)
	if err != nil {
		return err
	}

	switch args[0] {
	case "list":
		for _, u := range st.Users() {
			flag := ""
			if u.MustChangePassword {
				flag = "\tmust-change-password"
			}
			fmt.Printf("%s\t%s\t%s%s\n", u.Name, u.Role, u.Created.Format(time.RFC3339), flag)
		}
		return nil
	case "add":
		if *name == "" {
			return errors.New("-name is required")
		}
		pw, err := readPassword("Password for " + *name + ": ")
		if err != nil {
			return err
		}
		if _, err := st.AddUser(*name, store.Role(*role), pw); err != nil {
			return err
		}
		fmt.Printf("created %s (%s)\n", *name, *role)
		return nil
	case "passwd":
		if *name == "" {
			return errors.New("-name is required")
		}
		pw, err := readPassword("New password for " + *name + ": ")
		if err != nil {
			return err
		}
		if err := st.SetPassword(*name, pw); err != nil {
			return err
		}
		fmt.Printf("password changed for %s; their sessions are signed out\n", *name)
		return nil
	case "role":
		if *name == "" {
			return errors.New("-name is required")
		}
		if err := st.SetRole(*name, store.Role(*role)); err != nil {
			return err
		}
		fmt.Printf("%s is now %s\n", *name, *role)
		return nil
	case "rm":
		if *name == "" {
			return errors.New("-name is required")
		}
		if err := st.DeleteUser(*name); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", *name)
		return nil
	default:
		return fmt.Errorf("unknown user subcommand %q\n\n%w", args[0], usage())
	}
}

// readPassword takes $QS_PASSWORD if set, and otherwise reads one line from
// standard input.
//
// It does not turn off terminal echo, which would cost an external dependency
// for a path that exists only as a fallback to the web UI. Pipe the password in
// rather than typing it where someone can read the screen:
//
//	printf %s "$PW" | quicksurvey user add -name alice -role editor
func readPassword(prompt string) (string, error) {
	if pw := os.Getenv("QS_PASSWORD"); pw != "" {
		return pw, nil
	}
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func exportCmd(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dir := fs.String("data", env("QS_DATA_DIR", "/data"), "data directory ($QS_DATA_DIR)")
	id := fs.String("survey", "", "survey ID")
	kind := fs.String("kind", "responses", "responses or summary")
	tzName := fs.String("tz", env("QS_TZ", "Local"), "timezone for timestamps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("-survey is required")
	}
	loc, err := time.LoadLocation(*tzName)
	if err != nil {
		return err
	}
	st, err := openStore(*dir)
	if err != nil {
		return err
	}
	sv, ok := st.Survey(*id)
	if !ok {
		return fmt.Errorf("no survey with ID %q", *id)
	}
	switch *kind {
	case "responses":
		return export.Responses(os.Stdout, sv, st.Responses(sv.ID), loc)
	case "summary":
		results, voters := st.Tally(sv.ID)
		return export.Summary(os.Stdout, sv, results, voters)
	default:
		return fmt.Errorf("unknown export kind %q", *kind)
	}
}

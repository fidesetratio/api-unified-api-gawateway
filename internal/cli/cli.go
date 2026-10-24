// Package cli implements the gatewayctl operator commands. It is a library so it
// can be tested; cmd/gatewayctl only wires environment and process exit.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/store"
	"unifiedgw/migrations"
)

// Deps are the collaborators the commands need.
type Deps struct {
	Out, Err io.Writer
	Pool     *pgxpool.Pool
	Keys     *secrets.KeyRing // needed by key reencrypt only
	Getenv   func(string) string
	// Reconcile runs the expiry sweeper and the creating-payment reconciler once.
	Reconcile func(ctx context.Context) (expired, resumed int, err error)
}

const usage = `usage: gatewayctl <command> [flags]

commands:
  migrate up|status
  client create --name N --role caller|admin     (prints the API key once)
  client list                                     (id, name, role, status; never prints keys)
  client disable|enable|rotate-key --name N
  key reencrypt                                   (re-encrypt credentials under the active master key)
  review list
  review resolve --id N --resolution "why"
  reconcile run

Every command that changes data needs an operator identity:
  --operator NAME   or   GATEWAYCTL_OPERATOR=NAME
`

// Run executes one command and returns the process exit code.
func Run(ctx context.Context, args []string, d Deps) int {
	if len(args) == 0 {
		fmt.Fprint(d.Err, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "migrate":
		err = runMigrate(ctx, args[1:], d)
	case "client":
		err = runClient(ctx, args[1:], d)
	case "key":
		err = runKey(ctx, args[1:], d)
	case "review":
		err = runReview(ctx, args[1:], d)
	case "reconcile":
		err = runReconcile(ctx, args[1:], d)
	case "help", "-h", "--help":
		fmt.Fprint(d.Out, usage)
		return 0
	default:
		fmt.Fprintf(d.Err, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(d.Err, "error:", err)
		return 1
	}
	return 0
}

// operator resolves the required operator identity. It is attribution for the
// audit log (not authentication): the CLI already runs with database credentials.
func operator(fs *flag.FlagSet, flagVal string, d Deps) (store.Actor, map[string]any, error) {
	name := strings.TrimSpace(flagVal)
	if name == "" && d.Getenv != nil {
		name = strings.TrimSpace(d.Getenv("GATEWAYCTL_OPERATOR"))
	}
	if name == "" {
		return store.Actor{}, nil, errors.New("an operator identity is required: pass --operator NAME or set GATEWAYCTL_OPERATOR")
	}
	extra := map[string]any{}
	if u, err := user.Current(); err == nil {
		extra["os_user"] = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		extra["host"] = h
	}
	_ = fs
	return store.Actor{Type: "operator", ID: name}, extra, nil
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	op := fs.String("operator", "", "operator identity for the audit log")
	return fs, op
}

func runMigrate(ctx context.Context, args []string, d Deps) error {
	if len(args) == 0 {
		return errors.New("migrate needs up or status")
	}
	fs, op := newFlags("migrate " + args[0])
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "status":
		applied, err := store.MigrationVersions(ctx, d.Pool)
		if err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "%d migrations applied\n", len(applied))
		for _, v := range applied {
			fmt.Fprintln(d.Out, " ", v)
		}
		return nil
	case "up":
		who, extra, err := operator(fs, *op, d)
		if err != nil {
			return err
		}
		applied, err := store.Migrate(ctx, d.Pool, migrations.FS)
		if err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "applied %d migrations\n", len(applied))
		for _, v := range applied {
			fmt.Fprintln(d.Out, " ", v)
		}
		if len(applied) > 0 {
			extra["versions"] = applied
			return store.WriteAudit(ctx, d.Pool, who, "migrate.up", "", extra)
		}
		return nil
	}
	return fmt.Errorf("unknown migrate subcommand %q", args[0])
}

func runClient(ctx context.Context, args []string, d Deps) error {
	if len(args) == 0 {
		return errors.New("client needs create, disable, enable or rotate-key")
	}
	fs, op := newFlags("client " + args[0])
	name := fs.String("name", "", "client name")
	role := fs.String("role", auth.RoleCaller, "caller or admin")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "list" { // read-only: no operator needed, never prints keys
		rows, err := store.NewClients(d.Pool).List(ctx)
		if err != nil {
			return err
		}
		for _, c := range rows {
			fmt.Fprintf(d.Out, "%d\t%s\t%s\t%s\n", c.ID, c.Name, c.Role, c.Status)
		}
		return nil
	}
	who, extra, err := operator(fs, *op, d)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	clients := store.NewClients(d.Pool)

	switch args[0] {
	case "create":
		p, key, err := clients.Create(ctx, *name, *role)
		if err != nil {
			return err
		}
		if err := store.WriteAudit(ctx, d.Pool, who, "client.create", fmt.Sprint(p.ID),
			merge(extra, map[string]any{"name": *name, "role": *role})); err != nil {
			return err
		}
		fmt.Fprintf(d.Err, "Client %q (id %d, role %s) created. Store this key now; it is shown only once.\n", p.Name, p.ID, p.Role)
		fmt.Fprintln(d.Out, key)
		return nil
	case "rotate-key":
		key, err := clients.RotateKey(ctx, *name)
		if err != nil {
			return err
		}
		if err := store.WriteAudit(ctx, d.Pool, who, "client.rotate_key", *name, extra); err != nil {
			return err
		}
		fmt.Fprintln(d.Err, "New key issued; the old key no longer works. It is shown only once.")
		fmt.Fprintln(d.Out, key)
		return nil
	case "disable", "enable":
		status := map[string]string{"disable": "disabled", "enable": "active"}[args[0]]
		if err := clients.SetStatus(ctx, *name, status); err != nil {
			return err
		}
		if err := store.WriteAudit(ctx, d.Pool, who, "client."+args[0], *name, extra); err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "client %q is now %s\n", *name, status)
		return nil
	}
	return fmt.Errorf("unknown client subcommand %q", args[0])
}

func runKey(ctx context.Context, args []string, d Deps) error {
	if len(args) == 0 || args[0] != "reencrypt" {
		return errors.New("key needs: reencrypt")
	}
	fs, op := newFlags("key reencrypt")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	who, _, err := operator(fs, *op, d)
	if err != nil {
		return err
	}
	if d.Keys == nil {
		return errors.New("master keys are not configured (GATEWAY_MASTER_KEYS, GATEWAY_ACTIVE_KEY_ID)")
	}
	n, err := store.ReencryptAll(ctx, d.Pool, d.Keys, who)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "re-encrypted %d credential rows under key %q\n", n, d.Keys.ActiveID())
	return nil
}

func runReview(ctx context.Context, args []string, d Deps) error {
	if len(args) == 0 {
		return errors.New("review needs list or resolve")
	}
	fs, op := newFlags("review " + args[0])
	id := fs.Int64("id", 0, "review item id")
	resolution := fs.String("resolution", "", "what was decided and why")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "list":
		items, err := store.ListOpenReviews(ctx, d.Pool)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Fprintln(d.Out, "no open review items")
		}
		for _, it := range items {
			fmt.Fprintf(d.Out, "#%d  %-26s  uniquecode=%s  tx=%s\n    %s\n", it.ID, it.Kind, it.UniqueCode, it.TransactionID, it.Details)
		}
		return nil
	case "resolve":
		who, _, err := operator(fs, *op, d)
		if err != nil {
			return err
		}
		if *id <= 0 || strings.TrimSpace(*resolution) == "" {
			return errors.New("--id and --resolution are required")
		}
		if err := store.ResolveReview(ctx, d.Pool, *id, strings.TrimSpace(*resolution), who); err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "review item #%d resolved\n", *id)
		return nil
	}
	return fmt.Errorf("unknown review subcommand %q", args[0])
}

func runReconcile(ctx context.Context, args []string, d Deps) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("reconcile needs: run")
	}
	fs, op := newFlags("reconcile run")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	who, extra, err := operator(fs, *op, d)
	if err != nil {
		return err
	}
	if d.Reconcile == nil {
		return errors.New("reconcile is not available in this build")
	}
	expired, resumed, err := d.Reconcile(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "expired %d, resumed %d\n", expired, resumed)
	return store.WriteAudit(ctx, d.Pool, who, "reconcile.run", "", merge(extra, map[string]any{"expired": expired, "resumed": resumed}))
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

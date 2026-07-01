package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/spf13/cobra"
)

func newCertCmd() *cobra.Command {
	c := &cobra.Command{Use: "cert", Short: "Manage the HTTPS certificate the front door serves"}
	c.AddCommand(newCertInstallCmd())
	return c
}

func newCertInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Export the dotnet dev cert and load it into the running Caddy",
		Long: "The front door serves the host's dotnet HTTPS development certificate — the one `dotnet " +
			"dev-certs https --trust` already trusts — so no extra root CA is added. `init` does this on first " +
			"setup; run `cert install` to (re)export and reload it into a running Caddy (e.g. after the dev cert " +
			"is rotated or re-trusted). Trust the dotnet cert once with `dotnet dev-certs https --trust` if you " +
			"haven't.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			fmt.Println("• exporting the dotnet dev certificate…")
			if err := caddy.ExportDevCert(ctx); err != nil {
				return err
			}

			admin := caddy.NewAdmin()
			if err := admin.Ping(ctx); err != nil {
				return fmt.Errorf("caddy isn't running — run `%s init` first: %w", bin(), err)
			}
			body, err := caddy.TLSAppBody()
			if err != nil {
				return err
			}
			fmt.Println("• loading it into the running Caddy…")
			// POST replaces the value in place; PUT 409s if tls already exists and
			// DELETE+PUT stalls Caddy while it's serving HTTPS.
			if err := admin.Post(ctx, "/config/apps/tls", body); err != nil {
				return fmt.Errorf("load cert into Caddy: %w", err)
			}

			cert, _ := caddy.DevCertPath()
			fmt.Printf("✓ front door now serves the dotnet dev cert (%s) — no extra CA.\n", cert)
			return nil
		},
	}
}

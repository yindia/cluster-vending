package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/cli"
	"github.com/yindia/cluster-vending/pkg/cli/login"
	"github.com/yindia/cluster-vending/pkg/config"
)

// apiToken is the bearer token the API commands send: CV_TOKEN, else the
// stored login (refreshed when expired), else none.
func apiToken(ctx context.Context) (string, error) {
	store, err := login.DefaultStore()
	if err != nil {
		// No place for a login on this machine: only CV_TOKEN can apply.
		store = login.Store{}
	}
	return login.Source{Store: store}.Token(ctx)
}

func newLoginCmd() *cobra.Command {
	var o login.Options
	var deviceCode bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in at the API's OIDC issuer; the commands then send your token",
		Long: "Log in at the OIDC issuer the API accepts tokens from, and keep the\n" +
			"tokens in a file only you can read (token.json in the cluster-vending\n" +
			"directory of your configuration directory). Every command that calls the\n" +
			"API then sends the token, renewing it when it has expired.\n\n" +
			"By default the device code flow is used: you open a URL on any device\n" +
			"and enter a code, so it works over SSH. --browser logs in through the\n" +
			"browser of this machine instead.\n\n" +
			"Microsoft Entra ID: --entra-tenant, --entra-client-id (the CLI's public\n" +
			"client) and --entra-scope (the API's scope, api://<app-id>/.default)\n" +
			"stand for --issuer, --client-id and --scopes.\n\n" +
			"Automation sets $" + login.EnvToken + " to a token it got itself; it wins over the\n" +
			"stored login. See docs/authentication.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := login.DefaultStore()
			if err != nil {
				return err
			}
			o.Out, o.OpenBrowser = cmd.ErrOrStderr(), openBrowser
			creds, err := login.Login(cmd.Context(), o)
			if err != nil {
				return err
			}
			if err := store.Save(creds); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Logged in at %s. Tokens saved to %s\n", creds.Issuer, store.Path)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Issuer, "issuer", "", "OIDC issuer URL")
	f.StringVar(&o.ClientID, "client-id", "", "the CLI's public client at the issuer")
	f.StringSliceVar(&o.Scopes, "scopes", nil, "scopes to ask for (default openid,offline_access; with --entra-scope: that scope and offline_access)")
	f.BoolVar(&deviceCode, "device-code", true, "log in with a code entered on any device")
	f.BoolVar(&o.Browser, "browser", false, "log in through the browser of this machine")
	f.StringVar(&o.EntraTenant, "entra-tenant", "", "Microsoft Entra ID: the directory (tenant) ID")
	f.StringVar(&o.EntraClientID, "entra-client-id", "", "Microsoft Entra ID: the CLI's application (client) ID")
	f.StringVar(&o.EntraScope, "entra-scope", "", "Microsoft Entra ID: the API's scope, e.g. api://<app-id>/.default")
	f.BoolVar(&o.UseIDToken, "use-id-token", false, "send the ID token to the API, for issuers whose access tokens are not JWTs for it")
	cmd.MarkFlagsMutuallyExclusive("device-code", "browser")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := login.DefaultStore()
			if err != nil {
				return err
			}
			removed, err := store.Delete()
			if err != nil {
				return err
			}
			message := "Not logged in."
			if removed {
				message = "Logged out: " + store.Path + " removed."
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
			return err
		},
	}
}

func newWhoamiCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show who the API sees you as and what you may do (TenantService.WhoAmI)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadClient(changedFlags(cmd.Flags()), os.LookupEnv)
			if err != nil {
				return err
			}
			clients := cli.NewClients(cfg.Server, nil, cli.WithToken(apiToken))
			return cli.Run(cmd.Context(), cli.WhoAmI, clients, cli.Options{RequestTimeout: cfg.RequestTimeout}, cmd.OutOrStdout(), output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", cli.OutputTable, cli.OutputHelp)
	for _, s := range config.ClientSettings() {
		cmd.Flags().String(s.Flag, s.Default, fmt.Sprintf("%s [$%s]", s.Usage, s.Env))
	}
	return cmd
}

// openBrowser opens an http(s) URL in the user's browser.
func openBrowser(target string) error {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("not an http(s) URL")
	}
	name, args := "xdg-open", []string{u.String()}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", u.String()}
	}
	//nolint:gosec // the opener is fixed and the URL, checked above, is passed as an argument, not through a shell
	return exec.CommandContext(context.Background(), name, args...).Start()
}

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
	"golang.org/x/term"

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
	var secretEnv, secretFile string
	var secretPrompt bool
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
			"stored login. See docs/architecture.md#authentication.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var prompt func() (string, error)
			if secretPrompt {
				input, ok := cmd.InOrStdin().(*os.File)
				if !ok || !term.IsTerminal(int(input.Fd())) {
					return errors.New("--client-secret-prompt requires a terminal; use --client-secret-env or --client-secret-file otherwise")
				}
				prompt = func() (string, error) {
					_, _ = fmt.Fprint(cmd.ErrOrStderr(), "OIDC client secret: ")
					value, err := term.ReadPassword(int(input.Fd()))
					_, _ = fmt.Fprintln(cmd.ErrOrStderr())
					defer clear(value)
					return string(value), err
				}
			}
			secret, err := login.ReadClientSecret(secretEnv, secretFile, prompt)
			if err != nil {
				return err
			}
			o.ClientSecret = secret
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
	f.StringVar(&o.ClientID, "client-id", "", "the CLI's client ID at the issuer (public by default)")
	f.StringVar(&secretEnv, "client-secret-env", "", "environment variable NAME containing the optional confidential client secret; requires --browser")
	f.StringVar(&secretFile, "client-secret-file", "", "private file containing the optional confidential client secret; requires --browser")
	f.BoolVar(&secretPrompt, "client-secret-prompt", false, "read the optional confidential client secret without terminal echo; requires --browser")
	f.StringVar(&o.RedirectHost, "redirect-host", "localhost", "browser callback loopback host: localhost, 127.0.0.1 or ::1")
	f.IntVar(&o.CallbackPort, "callback-port", 0, "browser callback port (0 chooses an ephemeral port); path is /callback")
	f.StringSliceVar(&o.Scopes, "scopes", nil, "scopes to ask for (default openid,offline_access; with --entra-scope: that scope and offline_access)")
	f.BoolVar(&deviceCode, "device-code", true, "log in with a code entered on any device")
	f.BoolVar(&o.Browser, "browser", false, "log in through the browser of this machine")
	f.StringVar(&o.EntraTenant, "entra-tenant", "", "Microsoft Entra ID: the directory (tenant) ID")
	f.StringVar(&o.EntraClientID, "entra-client-id", "", "Microsoft Entra ID: the CLI's application (client) ID")
	f.StringVar(&o.EntraScope, "entra-scope", "", "Microsoft Entra ID: the API's scope, e.g. api://<app-id>/.default")
	f.BoolVar(&o.UseIDToken, "use-id-token", false, "send the ID token to the API, for issuers whose access tokens are not JWTs for it")
	cmd.MarkFlagsMutuallyExclusive("device-code", "browser")
	cmd.MarkFlagsMutuallyExclusive("client-secret-env", "client-secret-file", "client-secret-prompt")
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

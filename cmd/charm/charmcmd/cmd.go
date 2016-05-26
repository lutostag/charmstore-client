// Copyright 2014 Canonical Ltd.
// Licensed under the GPLv3, see LICENCE file for details.

package charmcmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/juju/cmd"
	"github.com/juju/idmclient/ussologin"
	"github.com/juju/juju/juju/osenv"
	"github.com/juju/loggo"
	"github.com/juju/persistent-cookiejar"
	"github.com/juju/usso"
	"golang.org/x/net/publicsuffix"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/charmrepo.v2-unstable/csclient"
	"gopkg.in/juju/charmrepo.v2-unstable/csclient/params"
	esform "gopkg.in/juju/environschema.v1/form"
	"gopkg.in/macaroon-bakery.v1/httpbakery"
	hbform "gopkg.in/macaroon-bakery.v1/httpbakery/form"
	"launchpad.net/gnuflag"
)

var logger = loggo.GetLogger("charm.cmd.charm")

const (
	// cmdName holds the name of the super command.
	cmdName = "charm"

	// cmdDoc holds the super command description.
	cmdDoc = `
The charm command provides commands and tools
that access the Juju charm store.
`
)

// New returns a command that can execute charm commands.
func New() *cmd.SuperCommand {
	// Maintain a map of the registered commands so that we can
	// avoid registering plugins with those names.
	names := map[string]bool{
		"help": true,
	}
	var c *cmd.SuperCommand
	notifyHelp := func(arg []string) {
		if len(arg) == 0 {
			registerPlugins(c, names)
		}
	}

	c = cmd.NewSuperCommand(cmd.SuperCommandParams{
		Name:            cmdName,
		Doc:             cmdDoc,
		Purpose:         "tools for accessing the charm store",
		MissingCallback: runPlugin,
		Log: &cmd.Log{
			DefaultConfig: os.Getenv(osenv.JujuLoggingConfigEnvKey),
		},
		NotifyHelp: notifyHelp,
	})
	register := func(subc cmd.Command) {
		names[subc.Info().Name] = true
		c.Register(subc)
	}
	register(&attachCommand{})
	register(&grantCommand{})
	register(&listCommand{})
	register(&loginCommand{})
	register(&logoutCommand{})
	register(&publishCommand{})
	register(&pullCommand{})
	register(&pushCommand{})
	register(&revokeCommand{})
	register(&setCommand{})
	register(&showCommand{})
	register(&termsCommand{})
	register(&whoamiCommand{})
	register(&listResourcesCommand{})
	c.AddHelpTopicCallback(
		"plugins",
		"Show "+c.Name+" plugins",
		func() string {
			return pluginHelpTopic(names)
		},
	)
	return c
}

// Expose the charm store server URL so that
// it can be changed for testing purposes.
var csclientServerURL = csclient.ServerURL

// serverURL returns the charm store server URL.
// The returned value can be overridden by setting the JUJU_CHARMSTORE
// environment variable.
func serverURL() string {
	if url := os.Getenv("JUJU_CHARMSTORE"); url != "" {
		return url
	}
	return csclientServerURL
}

// csClient embeds a charm store client and holds its associated HTTP client
// and cookie jar.
type csClient struct {
	*csclient.Client
	jar  *cookiejar.Jar
	ctxt *cmd.Context
}

// SaveJAR calls save on the jar member variable. This follows the Law
// of Demeter and allows csClient to meet interfaces.
func (c *csClient) SaveJAR() error {
	return c.jar.Save()
}

// newCharmStoreClient creates and return a charm store client with access to
// the associated HTTP client and cookie jar used to save authorization
// macaroons. If authUsername and authPassword are provided, the resulting
// client will use HTTP basic auth with the given credentials.
func newCharmStoreClient(ctxt *cmd.Context, auth authInfo) (*csClient, error) {
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
		Filename:         cookiejar.DefaultCookieFile(),
	})
	if err != nil {
		return nil, errgo.New("cannot load the cookie jar")
	}
	bakeryClient := httpbakery.NewClient()
	bakeryClient.Jar = jar
	tokenStore := ussologin.NewFileTokenStore(ussoTokenPath())
	filler := &esform.IOFiller{
		In:  ctxt.Stdin,
		Out: ctxt.Stdout,
	}
	bakeryClient.WebPageVisitor = httpbakery.NewMultiVisitor(
		ussologin.NewVisitor("charm", filler, tokenStore),
		hbform.Visitor{filler},
		httpbakery.WebBrowserVisitor,
	)
	csClient := csClient{
		Client: csclient.New(csclient.Params{
			URL:          serverURL(),
			BakeryClient: bakeryClient,
			User:         auth.username,
			Password:     auth.password,
		}),
		jar:  jar,
		ctxt: ctxt,
	}
	return &csClient, nil
}

// addAuthFlag adds the authentication flag to the given flag set.
func addAuthFlag(f *gnuflag.FlagSet, info *authInfo) {
	f.Var(info, "auth", "user:passwd to use for basic HTTP authentication")
}

// addChannelFlag adds the -c (--channel) flags to the given flag set.
func addChannelFlag(f *gnuflag.FlagSet, s *string) {
	f.StringVar(s, "c", "", fmt.Sprintf("the channel the charm or bundle is assigned to (%s|%s|%s)", params.StableChannel, params.DevelopmentChannel, params.UnpublishedChannel))
	f.StringVar(s, "channel", "", "")
}

type authInfo struct {
	username string
	password string
}

// Set implements gnuflag.Value.Set by validating
// the authentication flag.
func (a *authInfo) Set(s string) error {
	if s == "" {
		*a = authInfo{}
		return nil
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return errgo.New(`invalid auth credentials: expected "user:passwd"`)
	}
	if parts[0] == "" {
		return errgo.Newf("empty username")
	}
	a.username, a.password = parts[0], parts[1]
	return nil
}

// String implements gnuflag.Value.String.
func (a *authInfo) String() string {
	if a.username == "" && a.password == "" {
		return ""
	}
	return a.username + ":" + a.password
}

func ussoTokenPath() string {
	return osenv.JujuXDGDataHomePath("store-usso-token")
}

// translateError translates err into a new error with a more
// understandable error message. If err is not translated then it will be
// returned unchanged.
func translateError(err error) error {
	if err == nil {
		return err
	}
	cause := errgo.Cause(err)
	switch {
	case httpbakery.IsInteractionError(cause):
		err := translateInteractionError(cause.(*httpbakery.InteractionError))
		return errgo.Notef(err, "login failed")
	}
	return err
}

// translateInteractionError translates err into a new error with a user
// understandable error message.
func translateInteractionError(err *httpbakery.InteractionError) error {
	ussoError, ok := errgo.Cause(err.Reason).(*usso.Error)
	if !ok {
		return err.Reason
	}
	if ussoError.Code != "INVALID_DATA" {
		return ussoError
	}
	for k, v := range ussoError.Extra {
		// Only report the first error, this will be an arbitrary
		// field from the extra information. In general the extra
		// information only contains one item.
		if k == "email" {
			// Translate email to username so that it matches the prompt.
			k = "username"
		}
		if v1, ok := v.([]interface{}); ok && len(v1) > 0 {
			v = v1[0]
		}
		return errgo.Newf("%s: %s", k, v)
	}
	return ussoError
}

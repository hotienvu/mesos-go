package httpsched

import (
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/mesos/mesos-go/api/v1/lib"
	"github.com/mesos/mesos-go/api/v1/lib/backoff"
	"github.com/mesos/mesos-go/api/v1/lib/encoding"
	"github.com/mesos/mesos-go/api/v1/lib/httpcli"
	"github.com/mesos/mesos-go/api/v1/lib/httpcli/apierrors"
	"github.com/mesos/mesos-go/api/v1/lib/scheduler"
	"github.com/mesos/mesos-go/api/v1/lib/scheduler/calls"
)

var (
	errNotHTTPCli  = httpcli.ProtocolError("expected an httpcli.Response object, found something else instead")
	errBadLocation = httpcli.ProtocolError("failed to build new Mesos service endpoint URL from Location header")

	DefaultRedirectSettings = RedirectSettings{
		MaxAttempts:      9,
		MaxBackoffPeriod: 13 * time.Second,
		MinBackoffPeriod: 500 * time.Millisecond,
	}
)

type (
	RedirectSettings struct {
		MaxAttempts      int           // per httpDo invocation
		MaxBackoffPeriod time.Duration // should be more than minBackoffPeriod
		MinBackoffPeriod time.Duration // should be less than maxBackoffPeriod
	}

	client struct {
		*httpcli.Client
		redirect RedirectSettings
	}

	// Caller is the public interface a framework scheduler's should consume
	Caller interface {
		calls.Caller
		// httpDo is intentionally package-private; clients of this package may extend a Caller
		// generated by this package by overriding the Call func but may not customize httpDo.
		httpDo(encoding.Marshaler, ...httpcli.RequestOpt) (mesos.Response, error)
	}

	callerInternal interface {
		Caller
		// WithTemporary configures the Client with the temporary option and returns the results of
		// invoking f(). Changes made to the Client by the temporary option are reverted before this
		// func returns.
		WithTemporary(opt httpcli.Opt, f func() error) error
	}

	// Option is a functional configuration option type
	Option func(*client) Option

	callerTemporary struct {
		callerInternal                      // delegate actually does the work
		requestOpts    []httpcli.RequestOpt // requestOpts are temporary per-request options
		opt            httpcli.Opt          // opt is a temporary client option
	}
)

func (ct *callerTemporary) httpDo(m encoding.Marshaler, opt ...httpcli.RequestOpt) (resp mesos.Response, err error) {
	ct.callerInternal.WithTemporary(ct.opt, func() error {
		if len(opt) == 0 {
			opt = ct.requestOpts
		} else if len(ct.requestOpts) > 0 {
			opt = append(opt[:], ct.requestOpts...)
		}
		resp, err = ct.callerInternal.httpDo(m, opt...)
		return nil
	})
	return
}

func (ct *callerTemporary) Call(call *scheduler.Call) (resp mesos.Response, err error) {
	ct.callerInternal.WithTemporary(ct.opt, func() error {
		resp, err = ct.callerInternal.Call(call)
		return nil
	})
	return
}

// MaxRedirects is a functional option that sets the maximum number of per-call HTTP redirects for a scheduler client
func MaxRedirects(mr int) Option {
	return func(c *client) Option {
		old := c.redirect.MaxAttempts
		c.redirect.MaxAttempts = mr
		return MaxRedirects(old)
	}
}

// NewCaller returns a scheduler API Client in the form of a Caller. Concurrent invocations
// of Call upon the returned caller are safely executed in a serial fashion. It is expected that
// there are no other users of the given Client since its state may be modified by this impl.
func NewCaller(cl *httpcli.Client, opts ...Option) calls.Caller {
	result := &client{Client: cl, redirect: DefaultRedirectSettings}
	cl.With(result.redirectHandler())
	for _, o := range opts {
		if o != nil {
			o(result)
		}
	}
	return &state{
		client: result,
		fn:     disconnectedFn,
	}
}

// httpDo decorates the inherited behavior w/ support for HTTP redirection to follow Mesos leadership changes.
// NOTE: this implementation will change the state of the client upon Mesos leadership changes.
func (cli *client) httpDo(m encoding.Marshaler, opt ...httpcli.RequestOpt) (resp mesos.Response, err error) {
	var (
		done            chan struct{} // avoid allocating these chans unless we actually need to redirect
		redirectBackoff <-chan struct{}
		getBackoff      = func() <-chan struct{} {
			if redirectBackoff == nil {
				done = make(chan struct{})
				redirectBackoff = backoff.Notifier(cli.redirect.MinBackoffPeriod, cli.redirect.MaxBackoffPeriod, done)
			}
			return redirectBackoff
		}
	)
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	for attempt := 0; ; attempt++ {
		resp, err = cli.Client.Do(m, opt...)
		redirectErr, ok := err.(*mesosRedirectionError)
		if !ok {
			return resp, err
		}
		if attempt < cli.redirect.MaxAttempts {
			log.Println("redirecting to " + redirectErr.newURL)
			cli.With(httpcli.Endpoint(redirectErr.newURL))
			<-getBackoff()
			continue
		}
		return
	}
}

// Call implements Client
func (cli *client) Call(call *scheduler.Call) (mesos.Response, error) {
	return cli.httpDo(call)
}

type mesosRedirectionError struct{ newURL string }

func (mre *mesosRedirectionError) Error() string {
	return "mesos server sent redirect to: " + mre.newURL
}

func isErrNotLeader(err error) bool {
	if err == nil {
		return false
	}
	apiErr, ok := err.(*apierrors.Error)
	return ok && apiErr.Code == apierrors.CodeNotLeader
}

// redirectHandler returns a config options that decorates the default response handling routine;
// it transforms normal Mesos redirect "errors" into mesosRedirectionErrors by parsing the Location
// header and computing the address of the next endpoint that should be used to replay the failed
// HTTP request.
func (cli *client) redirectHandler() httpcli.Opt {
	return httpcli.HandleResponse(func(hres *http.Response, err error) (mesos.Response, error) {
		resp, err := cli.HandleResponse(hres, err) // default response handler
		if err == nil || !isErrNotLeader(err) {
			return resp, err
		}
		// TODO(jdef) for now, we're tightly coupled to the httpcli package's Response type
		res, ok := resp.(*httpcli.Response)
		if !ok {
			if resp != nil {
				resp.Close()
			}
			return nil, errNotHTTPCli
		}
		log.Println("master changed?")
		location, ok := buildNewEndpoint(res.Header.Get("Location"), cli.Endpoint())
		if !ok {
			return nil, errBadLocation
		}
		res.Close()
		return nil, &mesosRedirectionError{location}
	})
}

func buildNewEndpoint(location, currentEndpoint string) (string, bool) {
	// TODO(jdef) refactor this
	// mesos v0.29 will actually send back fully-formed URLs in the Location header
	if location == "" {
		return "", false
	}
	// current format appears to be //x.y.z.w:port
	hostport, parseErr := url.Parse(location)
	if parseErr != nil || hostport.Host == "" {
		return "", false
	}
	current, parseErr := url.Parse(currentEndpoint)
	if parseErr != nil {
		return "", false
	}
	current.Host = hostport.Host
	return current.String(), true
}

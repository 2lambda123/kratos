package sentry

import (
	"context"
	"net"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	http2 "github.com/go-kratos/kratos/v2/transport/http"
)

const valuesKey = "sentry"

type Option func(*options)

type options struct {
	repanic         bool
	waitForDelivery bool
	timeout         time.Duration
	tags            map[string]interface{}
}

// Repanic configures whether Sentry should repanic after recovery, in most cases it should be set to true.
func WithRepanic(repanic bool) Option {
	return func(opts *options) {
		opts.repanic = repanic
	}
}

// WaitForDelivery configures whether you want to block the request before moving forward with the response.
func WithWaitForDelivery(waitForDelivery bool) Option {
	return func(opts *options) {
		opts.waitForDelivery = waitForDelivery
	}
}

// Timeout for the event delivery requests.
func WithTimeout(timeout time.Duration) Option {
	return func(opts *options) {
		opts.timeout = timeout
	}
}

// Global tags injection, the value type must be string or log.Valuer
func WithTags(kvs map[string]interface{}) Option {
	return func(opts *options) {
		opts.tags = kvs
	}
}

// Server returns a new server middleware for Sentry.
func Server(opts ...Option) middleware.Middleware {
	conf := options{repanic: true}
	for _, o := range opts {
		o(&conf)
	}
	if conf.timeout == 0 {
		conf.timeout = 2 * time.Second
	}
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (reply interface{}, err error) {
			hub := sentry.GetHubFromContext(ctx)
			if hub == nil {
				hub = sentry.CurrentHub().Clone()
			}
			scope := hub.Scope()

			for k, v := range conf.tags {
				switch val := v.(type) {
				case string:
					scope.SetTag(k, val)
				case log.Valuer:
					if vs, ok := val(ctx).(string); ok {
						scope.SetTag(k, vs)
					}
				}
			}

			if tr, ok := transport.FromServerContext(ctx); ok {
				switch tr.Kind() {
				case transport.KindGRPC:
					gtr := tr.(*grpc.Transport)
					scope.SetContext("gRPC", map[string]interface{}{
						"endpoint":  gtr.Endpoint(),
						"operation": gtr.Operation(),
					})
					headers := make(map[string]interface{})
					for _, k := range gtr.RequestHeader().Keys() {
						headers[k] = gtr.RequestHeader().Get(k)
					}
					scope.SetContext("Headers", headers)
				case transport.KindHTTP:
					htr := tr.(*http2.Transport)
					r := htr.Request()
					scope.SetRequest(r)
				}
			}

			ctx = context.WithValue(ctx, valuesKey, hub)
			defer recoverWithSentry(conf, hub, ctx, req)
			return handler(ctx, req)
		}
	}
}

func recoverWithSentry(opts options, hub *sentry.Hub, ctx context.Context, req interface{}) {
	if err := recover(); err != nil {
		if !isBrokenPipeError(err) {
			eventID := hub.RecoverWithContext(
				context.WithValue(ctx, sentry.RequestContextKey, req),
				err,
			)
			if eventID != nil && opts.waitForDelivery {
				hub.Flush(opts.timeout)
			}
		}
		if opts.repanic {
			panic(err)
		}
	}
}

func isBrokenPipeError(err interface{}) bool {
	if netErr, ok := err.(*net.OpError); ok {
		if sysErr, ok := netErr.Err.(*os.SyscallError); ok {
			if strings.Contains(strings.ToLower(sysErr.Error()), "broken pipe") ||
				strings.Contains(strings.ToLower(sysErr.Error()), "connection reset by peer") {
				return true
			}
		}
	}
	return false
}

// GetHubFromContext retrieves attached *sentry.Hub instance from context.
func GetHubFromContext(ctx context.Context) *sentry.Hub {
	if hub, ok := ctx.Value(valuesKey).(*sentry.Hub); ok {
		return hub
	}
	return nil
}

# elagoht/errortrack

A collage plugin that reports server errors to [Sentry](https://sentry.io), or to
any service that speaks the Sentry protocol (GlitchTip, self-hosted Sentry), with
the standard library alone. It hears every `5xx` and every recovered panic through
collage's `ErrorHook`, builds an event, and sends it from a background goroutine,
so a slow or down Sentry never slows a request.

```sh
go get github.com/Elagoht/collage-errortrack
```

```json
{
  "elagoht/errortrack": {
    "dsnEnv": "SENTRY_DSN",
    "environment": "production",
    "sampleRate": 1
  }
}
```

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{
		errortrack.New(errortrack.Options{
			User: func(r *http.Request) errortrack.User {
				return errortrack.User{ID: userID(r)}
			},
		}),
	},
})
```

Set `SENTRY_DSN` to your project's DSN, `https://<key>@o0.ingest.sentry.io/0`.
The DSN is a credential: keep it in the environment, not in `collage.json`. Errors
the plugin returns never carry it.

Requires collage v0.45.0 or later (for `ErrorHook`). Register it in
`Config.Plugins`, where `Configure` runs.

## Options

| Option | JSON | Default | |
| --- | --- | --- | --- |
| `DSNEnv` | `dsnEnv` | | Name of the environment variable holding the DSN. Startup fails if it is empty |
| `DSN` | not configurable | | The DSN, set from Go. One of `DSN` or `DSNEnv` is required; `DSNEnv` wins |
| `Environment` | `environment` | `"development"` in dev mode, else `"production"` | Sentry's environment tag |
| `Release` | `release` | the build ID | Sentry's release |
| `MinStatus` | `minStatus` | `500` | Lowest status reported. Panics are always reported |
| `SampleRate` | `sampleRate` | `1` | Fraction of events sent, in (0, 1]; `0` means all |
| `PerMinute` | `perMinute` | `60` | Events sent per minute at most; the rest are dropped |
| `QueueSize` | `queueSize` | `100` | Events waiting to be sent; a full queue drops new ones |
| `InDevelopment` | `inDevelopment` | `false` | Without it nothing is sent in dev mode |
| `Timeout` | `timeout` | `"5s"` | Per send. A Go duration string, or nanoseconds |
| `SendPath` | `sendPath` | `false` | The raw path instead of the route pattern |
| `SendQuery` | `sendQuery` | `false` | Query values instead of `[filtered]` |
| `SendIP` | `sendIP` | `false` | The client address |
| `User` | not configurable | | `func(r *http.Request) User`. Go only |
| `BeforeSend` | not configurable | | `func(e *Event) bool`; `false` drops the event. Go only |
| `HTTPClient` | not configurable | | Your own client for the send. Go only |

A negative `minStatus`, `perMinute`, `queueSize` or `timeout`, or a `sampleRate`
outside [0, 1], fails startup.

## What is sent

| Sent | Default | Never sent |
| --- | --- | --- |
| The error chain: each link's Go type and message, outermost last (at most 10) | always | |
| A panic's stack frames, with `in_app` marking your module | always | |
| Level: `error`, or `fatal` for a panic | always | |
| Tags `stage`, `route.kind`, `method`, `status` | always | |
| The route pattern (`/posts/{slug}`) as transaction and in the URL | the raw path only with `SendPath` | |
| Query keys, each value as `[filtered]` | the values only with `SendQuery` | |
| Request headers `User-Agent`, `Accept`, `Accept-Language`, `Content-Type`, `Content-Length` | always | every other header: `Cookie`, `Authorization`, CSRF tokens... |
| `Referer`, without credentials, query and fragment | always | |
| The client address | only with `SendIP` | |
| A user ID, username and email | only what your `User` callback returns | |
| Environment, release, server host name | always | |
| | | The request body and form values |

The tags are `stage` (where collage met the failure, or `capture` for `Capture`),
`route.kind` (when a route resolved), `method` and `status` (when known).

## Error messages may carry secrets

A message is whatever the failing code wrote into its error, and it is sent as is:
a database driver may echo a query, a parser the input it choked on. Scrub with
`BeforeSend`, which may edit the event or return `false` to drop it:

```go
var secret = regexp.MustCompile(`password=[^&\s]+`)

errortrack.New(errortrack.Options{
	BeforeSend: func(e *errortrack.Event) bool {
		for i := range e.Exceptions {
			e.Exceptions[i].Value = secret.ReplaceAllString(e.Exceptions[i].Value, "password=[filtered]")
		}
		return true
	},
})
```

A panic in `BeforeSend` or `User` is recovered and logged; `BeforeSend` dropping
the event, `User` leaving it without a user.

## Who it happened to

`User` runs for each event that has a request. Send only what you need to find the
reader again, an ID being enough:

```go
User: func(r *http.Request) errortrack.User {
	return errortrack.User{ID: userID(r)}
},
```

`userID` is your own session lookup. `Username` and `Email` are there if you want
them in Sentry.

## Jobs outside a request

`Capture` reports an error that happened outside a request, such as a background
job. The event has no request and an unknown status, so `MinStatus` never filters
it; its transaction is the route the context carries, if any.

```go
et := errortrack.New(errortrack.Options{DSNEnv: "SENTRY_DSN"})

go func() {
	if err := nightlyReport(ctx); err != nil {
		et.Capture(ctx, err)
	}
}()
```

Keep the plugin you register and call `Capture` on it. A nil error does nothing.

## GlitchTip and self-hosted Sentry

Use the project's DSN as is. The plugin posts to `{scheme}://{host}/{path}api/{project}/envelope/`
taken from it, so a DSN with a path prefix (`https://<key>@errors.example.com/sentry/3`)
works too. The project must be a number.

## Limits

- Errors only: `5xx` and panics, and a status below `500` only if you lower
  `MinStatus`. There is no performance data and no sessions.
- No retries. A failed send, an event dropped because the queue is full, the
  `PerMinute` cap or a pause is counted and logged, at most once a minute.
- A `429` pauses all sending for as long as `X-Sentry-Rate-Limits` or `Retry-After`
  says, else a minute. The rate-limit categories are not told apart: any limit
  pauses everything. The headers are read only on a `429`.
- A fragment that is not `Required` and fails is absorbed by collage and does not
  produce a `5xx`, so it is not reported.
- An `error_page` event, an error page that itself failed to render, is reported
  with status `500`.
- `Shutdown` sends what is queued until its context ends; what is left then is
  dropped.

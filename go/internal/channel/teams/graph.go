package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	apltransport "github.com/muthuishere/agent-conversations/go/internal/transport/apl"
)

// ---------------------------------------------------------------------------
// Graph wire types — only the fields this adapter actually uses.
//
// Everything else survives in Message.Raw, because ARCHITECTURE.md §5.2 is
// right: you will need a field you did not anticipate, and a struct that
// discards the payload has already lost it.
// ---------------------------------------------------------------------------

type graphUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type graphFrom struct {
	User *graphUser `json:"user"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphMention struct {
	Mentioned *graphFrom `json:"mentioned"`
}

type graphChannelIdentity struct {
	TeamID    string `json:"teamId"`
	ChannelID string `json:"channelId"`
}

// graphMessage keeps the decoded fields AND the bytes they came from, because
// the canonical envelope carries `raw` and dropping it here would make that
// impossible to honour.
type graphMessage struct {
	ID              string                `json:"id"`
	ReplyToID       *string               `json:"replyToId"`
	MessageType     string                `json:"messageType"`
	CreatedDateTime string                `json:"createdDateTime"`
	DeletedDateTime *string               `json:"deletedDateTime"`
	ChatID          *string               `json:"chatId"`
	ChannelIdentity *graphChannelIdentity `json:"channelIdentity"`
	From            *graphFrom            `json:"from"`
	Body            graphBody             `json:"body"`
	Mentions        []graphMention        `json:"mentions"`

	// raw is the untouched payload; unexported so it never round-trips back
	// out as a field of its own.
	raw json.RawMessage
}

func (m *graphMessage) UnmarshalJSON(b []byte) error {
	type alias graphMessage // break the recursion
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = graphMessage(a)
	m.raw = append(json.RawMessage(nil), b...)
	return nil
}

type graphChannel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type graphTeam struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type graphChatMember struct {
	DisplayName string `json:"displayName"`
	UserID      string `json:"userId"`
}

type graphChat struct {
	ID       string            `json:"id"`
	Topic    *string           `json:"topic"`
	ChatType string            `json:"chatType"`
	Members  []graphChatMember `json:"members"`
}

// collection is the shape every Graph list endpoint returns: a `value` array,
// an optional paging link, and — on a delta endpoint — the link carrying the
// next delta token.
type collection[T any] struct {
	Value     []T    `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// httpStatusError carries the status of a non-retryable 4xx so a caller can
// distinguish "the server rejected THIS request" from everything else. It
// exists for one reason: real Graph sometimes hands back an @odata.nextLink
// that it then refuses (HTTP 400, "Parameter 'DeltaToken' not supported for
// this request") — observed live, not reproducible on the simulator. A caller
// following a server-supplied continuation needs to tell that apart from a
// genuinely bad first request.
type httpStatusError struct {
	status int
	err    error
}

func (e *httpStatusError) Error() string { return e.err.Error() }
func (e *httpStatusError) Unwrap() error { return e.err }

// graphError is Graph's error envelope. We keep the `code` because it is the
// stable machine-readable half; the message is for a human.
type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// The HTTP layer
// ---------------------------------------------------------------------------

// get fetches and decodes one Graph collection, following `@odata.nextLink`
// until the server stops offering one. Paging is the channel's job: nothing
// above the transport seam should ever see a continuation token.
func getCollection[T any](ctx context.Context, c *Channel, path string) ([]T, string, error) {
	var out []T
	next := path
	deltaLink := ""
	for hop := 0; next != "" && hop < maxPageHops; hop++ {
		var page collection[T]
		if err := c.do(ctx, http.MethodGet, next, nil, &page); err != nil {
			// A 400 on a CONTINUATION hop means Graph gave us a link it will
			// not honour. Keep what we already collected and treat the page
			// as terminal; failing the whole conversation for the server's
			// own bad link took 30 of 38 real conversations down in one run.
			// A 400 on the FIRST hop is still a real error and still fails.
			var hse *httpStatusError
			if hop > 0 && errors.As(err, &hse) && hse.status == http.StatusBadRequest {
				break
			}
			return nil, "", err
		}
		out = append(out, page.Value...)
		if page.DeltaLink != "" {
			deltaLink = page.DeltaLink
		}
		next = page.NextLink
	}
	return out, deltaLink, nil
}

const (
	maxPageHops   = 50
	maxRetries    = 4
	baseBackoff   = 250 * time.Millisecond
	defaultScan   = 10
	cursorVersion = 1
)

// do performs one request with the retry policy a real tenant forces on you.
//
// Teams/Graph throttles hard and answers 429 with a `Retry-After` header.
// Honouring it is not politeness: ignoring it gets the app-wide quota cut, and
// a channel that retries in a tight loop takes the whole listener down with it
// (ARCHITECTURE.md §5.6). 5xx gets the same treatment because a transient
// gateway error is indistinguishable from a throttle at this layer.
func (c *Channel) do(ctx context.Context, method, rawURL string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return convo.Wrap(convo.ErrInternal, "encoding request body: %v", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.backoffFor(lastErr, attempt)
			select {
			case <-ctx.Done():
				return convo.Wrap(convo.ErrTimeout, "context cancelled while backing off")
			case <-time.After(delay):
			}
		}

		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
		if err != nil {
			return convo.Wrap(convo.ErrNotConfigured, "bad request url %q: %v", rawURL, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if c.cfg.Authorization != "" {
			req.Header.Set("Authorization", c.cfg.Authorization)
		}
		if c.cfg.UserName != "" {
			// A MOCK affordance. The Graph-shaped simulator resolves identity
			// from this header so a test needs no OAuth dance; a real tenant
			// ignores it and uses the bearer token above. It is set explicitly
			// rather than silently so nobody mistakes it for authentication.
			req.Header.Set("x-user-name", c.cfg.UserName)
		}

		resp, err := c.client().Do(req)
		if err != nil {
			// An identity the transport cannot satisfy is not a transient
			// failure. Retrying it four times turns one actionable message
			// ("run this login command") into four, delays it by four seconds,
			// and reports it as an unreachable host — which sends the reader
			// looking at the network instead of at their own grant.
			var ae *apltransport.AuthError
			if errors.As(err, &ae) {
				return convo.Wrap(convo.ErrNotConfigured, "%s %s: %v", method, redact(rawURL), err)
			}
			lastErr = convo.Wrap(convo.ErrHostUnavailable, "%s %s: %v", method, redact(rawURL), err)
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = convo.Wrap(convo.ErrHostUnavailable, "reading response: %v", readErr)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = &retryable{
				after: retryAfter(resp.Header.Get("Retry-After")),
				err: convo.Wrap(convo.ErrHostUnavailable, "%s %s: HTTP %d",
					method, redact(rawURL), resp.StatusCode),
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			return convo.Wrap(convo.ErrNotConfigured, "%s: %s",
				redact(rawURL), graphMessageOf(respBody, "not found"))
		case resp.StatusCode >= 400:
			return &httpStatusError{status: resp.StatusCode, err: convo.Wrap(convo.ErrInternal, "%s %s: HTTP %d: %s",
				method, redact(rawURL), resp.StatusCode, graphMessageOf(respBody, "request failed"))}
		}

		if out == nil {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return convo.Wrap(convo.ErrInternal, "decoding %s: %v", redact(rawURL), err)
		}
		return nil
	}
	if r, ok := lastErr.(*retryable); ok {
		// Returned AS the retryable, not unwrapped: errors.Is/As still reach
		// the convo.Err inside, and the ingest loop can see through
		// convo.IsRetryable that this room was throttled, not broken, and back
		// its worker off instead of moving straight on to hammer the next one.
		return r
	}
	return lastErr
}

// retryable carries a server-suggested delay alongside the error it will
// eventually become.
type retryable struct {
	after time.Duration
	err   error
}

func (r *retryable) Error() string             { return r.err.Error() }
func (r *retryable) Unwrap() error             { return r.err }
func (r *retryable) Retryable() bool           { return true }
func (r *retryable) RetryAfter() time.Duration { return r.after }

func (c *Channel) backoffFor(last error, attempt int) time.Duration {
	if r, ok := last.(*retryable); ok && r.after > 0 {
		return r.after
	}
	d := baseBackoff << (attempt - 1) // 250ms, 500ms, 1s, 2s
	if max := 5 * time.Second; d > max {
		d = max
	}
	return d
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func graphMessageOf(body []byte, def string) string {
	var ge graphError
	if err := json.Unmarshal(body, &ge); err == nil && ge.Error.Code != "" {
		return fmt.Sprintf("%s: %s", ge.Error.Code, ge.Error.Message)
	}
	return def
}

// redact keeps a delta token out of an error string. Tokens are position, not
// a secret — but they are noise, and an error a human reads should be readable.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("$deltatoken") != "" {
		q.Set("$deltatoken", "…")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (c *Channel) client() Doer {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	return http.DefaultClient
}

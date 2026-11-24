// Package rabbitmq publishes events to RabbitMQ. Every publish is mandatory
// and confirmed (D49): a message counts as delivered only when the broker
// has routed it to at least one queue and confirmed that it stored it.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/thiagogenez/labflow/libs/go/config"
)

const (
	// Exchange is the topic exchange every event is published to, with the
	// event type as routing key (D62).
	Exchange = "labflow.events"
	// MaxBatch bounds one Publish call. The channel buffers up to this many
	// returned messages; the client drops a return that finds the buffer
	// full, which would turn an unroutable message into a silent success.
	MaxBatch = 500
	// contentType marks a CloudEvent in structured mode (D47).
	contentType = "application/cloudevents+json"
	dialTimeout = 5 * time.Second
	// closeTimeout bounds closing a connection whose broker stopped
	// answering.
	closeTimeout = time.Second
)

var (
	errRefused = errors.New("broker refused the message")
	errNotSent = errors.New("not sent: an earlier message of the batch failed")
)

// URLFromEnv reads the broker settings shared by every producer: BROKER
// selects the adapter, and RabbitMQ is the only one until the Kafka adapter
// arrives; RABBITMQ_URL locates the broker. The URL holds a password, so no
// error message repeats it.
func URLFromEnv(env *config.Env) string {
	broker := env.Required("BROKER")
	env.Check(broker == "" || broker == "rabbitmq", fmt.Sprintf("BROKER must be rabbitmq, got %q", broker))
	raw := env.Required("RABBITMQ_URL")
	if raw != "" {
		env.Check(validURL(raw), "RABBITMQ_URL must be an amqp:// or amqps:// URL with a host and a valid port")
	}
	return raw
}

// validURL adds to amqp.ParseURI, which accepts a port above 65535 and
// turns a URL without a host into localhost.
func validURL(raw string) bool {
	if _, err := amqp.ParseURI(raw); err != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
			return false
		}
	}
	return true
}

// Message is one event to publish.
type Message struct {
	// ID is the event ID, sent as the AMQP message_id.
	ID string
	// RoutingKey is the event type.
	RoutingKey string
	// Body is the CloudEvent in structured JSON mode.
	Body []byte
	// TTL, when positive, is how long the message may wait in a queue
	// before the broker expires it, sent as the AMQP expiration.
	TTL time.Duration
}

// Publisher publishes on one confirmed channel. After any connection or
// channel failure it dials again on the next call. It is not safe for
// concurrent use.
type Publisher struct {
	url     string
	name    string
	conn    *amqp.Connection
	ch      *amqp.Channel
	returns chan amqp.Return
	closed  chan *amqp.Error

	mu sync.Mutex
	// socket is the TCP connection under conn. Closing it fails every call
	// still waiting on the broker, which no context reaches otherwise: a
	// send blocked on a full socket, as under a memory alarm, or a reply
	// that never comes.
	socket net.Conn
}

// NewPublisher returns a publisher that connects on first use. name labels
// the connection in RabbitMQ's management UI.
func NewPublisher(url, name string) *Publisher {
	return &Publisher{url: url, name: name}
}

// Publish sends the messages and waits until the broker settles each one or
// ctx ends. It returns one error per message, nil when the broker accepted
// it: routed to a queue and confirmed. The caller publishes the others
// again later.
func (p *Publisher) Publish(ctx context.Context, msgs []Message) []error {
	errs := make([]error, len(msgs))
	if len(msgs) > MaxBatch {
		return fill(errs, fmt.Errorf("batch of %d messages exceeds %d", len(msgs), MaxBatch))
	}
	defer p.abortWhenDone(ctx)()
	if err := p.connect(ctx); err != nil {
		return fill(errs, err)
	}
	confirms := make([]confirmation, 0, len(msgs))
	for i, m := range msgs {
		msg := amqp.Publishing{ContentType: contentType, DeliveryMode: amqp.Persistent, MessageId: m.ID, Body: m.Body}
		if m.TTL > 0 {
			msg.Expiration = strconv.FormatInt(max(m.TTL.Milliseconds(), 1), 10)
		}
		dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, Exchange, m.RoutingKey, true, false, msg)
		if err != nil {
			errs[i] = fmt.Errorf("send: %w", err)
			fill(errs[i+1:], errNotSent)
			break
		}
		confirms = append(confirms, dc)
	}
	broken := settle(ctx, confirms, errs) || len(confirms) < len(msgs)
	p.drainReturns(msgs, errs)
	if broken || p.ch.IsClosed() {
		// The broker closes the channel on a refused publish, for example
		// one the user's permissions do not allow; late confirms or returns
		// of an abandoned batch must not reach the next one.
		select {
		case reason := <-p.closed:
			if reason != nil {
				for i := range errs {
					if errs[i] != nil {
						errs[i] = fmt.Errorf("%w; channel closed: %w", errs[i], reason)
					}
				}
			}
		default:
		}
		p.Close()
	}
	return errs
}

// abortWhenDone closes the socket when ctx ends, until the returned release
// is called. Release waits for an abort already under way, so it can never
// close the socket of a later call.
func (p *Publisher) abortWhenDone(ctx context.Context) (release func()) {
	aborted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(aborted)
		p.closeSocket()
	})
	return func() {
		if !stop() {
			<-aborted
		}
	}
}

func (p *Publisher) closeSocket() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.socket != nil {
		_ = p.socket.Close() // the connection is being abandoned
	}
}

// confirmation is the part of amqp.DeferredConfirmation that settle reads.
type confirmation interface {
	Done() <-chan struct{}
	Acked() bool
}

// settle waits for each confirmation in turn until ctx ends, then records
// the outcome of every confirmation already settled, so a slow message
// never hides the acks of later ones. It reports whether any stayed
// unsettled, leaving the channel's state unknown.
func settle(ctx context.Context, confirms []confirmation, errs []error) bool {
	unsettled := false
	for i, c := range confirms {
		select {
		case <-c.Done():
		case <-ctx.Done():
		}
		select {
		case <-c.Done():
			if !c.Acked() {
				errs[i] = errRefused
			}
		default:
			errs[i] = fmt.Errorf("unsettled: %w", ctx.Err())
			unsettled = true
		}
	}
	return unsettled
}

// drainReturns marks the messages the broker returned as unroutable. The
// broker returns a message before it confirms it, so every return for this
// batch is already buffered.
func (p *Publisher) drainReturns(msgs []Message, errs []error) {
	for {
		select {
		case r, open := <-p.returns:
			if !open {
				return // the channel closed; its reason follows
			}
			for i := range msgs {
				if msgs[i].ID == r.MessageId && errs[i] == nil {
					errs[i] = fmt.Errorf("broker returned it for routing key %q: %s", r.RoutingKey, r.ReplyText)
				}
			}
		default:
			return
		}
	}
}

func (p *Publisher) connect(ctx context.Context) error {
	if p.ch != nil && !p.ch.IsClosed() {
		return nil
	}
	p.Close()
	props := amqp.NewConnectionProperties()
	props.SetClientConnectionName(p.name)
	conn, err := amqp.DialConfig(p.url, amqp.Config{Properties: props, Dial: func(network, addr string) (net.Conn, error) {
		socket, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		// Bounds the AMQP handshake, as amqp.DefaultDial does; the client
		// clears it once connected.
		if err := socket.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
			_ = socket.Close()
			return nil, err
		}
		p.mu.Lock()
		p.socket = socket
		p.mu.Unlock()
		// ctx may have ended before the socket existed, when no abort could
		// reach it.
		if err := ctx.Err(); err != nil {
			_ = socket.Close()
			return nil, err
		}
		return socket, nil
	}})
	if err != nil {
		p.Close()
		return fmt.Errorf("connect to rabbitmq: %w", err)
	}
	p.conn = conn
	ch, err := conn.Channel()
	if err == nil {
		err = ch.Confirm(false)
	}
	if err != nil {
		p.Close()
		return fmt.Errorf("open confirmed channel: %w", err)
	}
	p.ch = ch
	p.returns = ch.NotifyReturn(make(chan amqp.Return, MaxBatch))
	p.closed = ch.NotifyClose(make(chan *amqp.Error, 1))
	return nil
}

// Close closes the connection, if any, waiting at most closeTimeout for the
// broker to acknowledge it. The client's own close deadline is moved on by
// every heartbeat that arrives meanwhile, so the socket is closed instead.
func (p *Publisher) Close() {
	if p.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		release := p.abortWhenDone(ctx)
		_ = p.conn.Close() // closing a broken connection fails, and there is nothing left to recover
		release()
		cancel()
	}
	p.mu.Lock()
	p.socket = nil
	p.mu.Unlock()
	p.conn, p.ch, p.returns, p.closed = nil, nil, nil, nil
}

func fill(errs []error, err error) []error {
	for i := range errs {
		errs[i] = err
	}
	return errs
}

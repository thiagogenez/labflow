package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/thiagogenez/labflow/libs/go/config"
	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
	"github.com/thiagogenez/labflow/libs/go/rabbitmqtest"
)

const (
	queue      = "email-worker"
	deadLetter = "email-worker.dead-letter"
	placed     = "labflow.order.placed.v1"
)

func TestMain(m *testing.M) { os.Exit(rabbitmqtest.Main(m.Run)) }

func newPublisher(t *testing.T) *rabbitmq.Publisher {
	t.Helper()
	rabbitmqtest.Purge(t, queue, deadLetter)
	p := rabbitmq.NewPublisher(rabbitmqtest.URL(t, "order-api"), "test")
	t.Cleanup(p.Close)
	return p
}

func messages(key string, n, size int) []rabbitmq.Message {
	msgs := make([]rabbitmq.Message, n)
	for i := range msgs {
		msgs[i] = rabbitmq.Message{ID: fmt.Sprintf("m%d", i), RoutingKey: key, Body: make([]byte, size)}
	}
	return msgs
}

func TestPublishRoutesToTheConsumerQueue(t *testing.T) {
	p := newPublisher(t)
	body := `{"specversion":"1.0","id":"e1"}`
	msgs := []rabbitmq.Message{
		{ID: "e1", RoutingKey: placed, Body: []byte(body), TTL: time.Minute},
		{ID: "e2", RoutingKey: placed, Body: []byte(body)},
	}
	for _, err := range p.Publish(t.Context(), msgs) {
		if err != nil {
			t.Fatal(err)
		}
	}
	d := rabbitmqtest.Get(t, queue)
	if d.MessageId != "e1" || d.ContentType != "application/cloudevents+json" || d.Expiration != "60000" ||
		d.DeliveryMode != amqp.Persistent || d.Exchange != rabbitmq.Exchange || d.RoutingKey != placed || string(d.Body) != body {
		t.Errorf("delivery = %+v", d)
	}
	if d := rabbitmqtest.Get(t, queue); d.Expiration != "" {
		t.Errorf("expiration without a TTL = %q", d.Expiration)
	}
}

func TestPublishReportsEachMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want string
		// closes reports that the broker closes the channel, which loses
		// the confirm of an earlier message still in flight: that message
		// is unsettled and will be published again.
		closes bool
	}{
		{name: "no queue bound", key: "labflow.order.unbound.v1", want: "NO_ROUTE"},
		{name: "routing key of another producer", key: "labflow.payment.refunded.v1", want: "ACCESS_REFUSED", closes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPublisher(t)
			msgs := []rabbitmq.Message{
				{ID: "ok", RoutingKey: placed, Body: []byte(`{}`)},
				{ID: "bad", RoutingKey: tc.key, Body: []byte(`{}`)},
			}
			errs := p.Publish(t.Context(), msgs)
			if errs[1] == nil || !strings.Contains(errs[1].Error(), tc.want) {
				t.Errorf("error of bad = %v, want %s", errs[1], tc.want)
			}
			if !tc.closes && errs[0] != nil {
				t.Errorf("error of ok = %v", errs[0])
			}
			// The publisher recovers, by reconnecting where the broker
			// closed the channel.
			rabbitmqtest.Purge(t, queue)
			if errs := p.Publish(t.Context(), msgs[:1]); errs[0] != nil {
				t.Errorf("next Publish: %v", errs[0])
			}
		})
	}
}

// A full queue refuses new messages, so the relay keeps them in the outbox
// instead of losing them. A quorum queue accepts the messages already in
// flight when it fills, so the test fills it first and then expects a
// refusal within a few publishes.
func TestPublishReportsAFullQueue(t *testing.T) {
	p := newPublisher(t)
	admin := rabbitmqtest.Admin(t)
	const key = "labflow.order.full.v1"
	_, err := admin.QueueDeclare("test-full", true, false, false, false,
		amqp.Table{"x-queue-type": "quorum", "x-max-length": 1, "x-overflow": "reject-publish"})
	if err == nil {
		err = admin.QueueBind("test-full", key, rabbitmq.Exchange, false, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rabbitmqtest.Admin(t).QueueDelete("test-full", false, false, false) })
	msgs := messages(key, 20, 1)
	if errs := p.Publish(t.Context(), msgs[:2]); errs[0] != nil {
		t.Fatal(errs[0])
	}
	for _, m := range msgs[2:] {
		if err := p.Publish(t.Context(), []rabbitmq.Message{m})[0]; err != nil {
			if !strings.Contains(err.Error(), "broker refused") {
				t.Errorf("error = %v, want a refusal", err)
			}
			return
		}
	}
	t.Error("the full queue accepted every message")
}

func TestPublishRefusesAnOversizedBatch(t *testing.T) {
	p := newPublisher(t)
	errs := p.Publish(t.Context(), make([]rabbitmq.Message, rabbitmq.MaxBatch+1))
	if len(errs) != rabbitmq.MaxBatch+1 || errs[0] == nil {
		t.Errorf("Publish = %d results, first %v", len(errs), errs[0])
	}
}

// returnsWithin runs f and fails the test unless it returns within d.
func returnsWithin(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Errorf("still blocked after %v", d)
		// Wait for f, which shares the test's publisher, before cleanup runs.
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("never returned")
		}
	}
}

// Under a memory alarm the broker stops reading from publishers, so a large
// batch blocks on the full socket; Publish still returns by its deadline.
func TestPublishGivesUpUnderAMemoryAlarm(t *testing.T) {
	p := newPublisher(t)
	if errs := p.Publish(t.Context(), messages(placed, 1, 1)); errs[0] != nil {
		t.Fatal(errs[0])
	}
	rabbitmqtest.Exec(t, "rabbitmqctl", "set_vm_memory_high_watermark", "absolute", "10MB")
	t.Cleanup(func() { rabbitmqtest.Exec(t, "rabbitmqctl", "set_vm_memory_high_watermark", "0.6") })
	var errs []error
	returnsWithin(t, 5*time.Second, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		errs = p.Publish(ctx, messages(placed, rabbitmq.MaxBatch, 128<<10))
	})
	for i, err := range errs {
		if err == nil {
			t.Fatalf("message %d accepted under the alarm", i)
		}
	}
}

// Closing a connection to a hung broker waits about a second, not for the
// broker, so shutdown stays bounded.
func TestCloseReturnsWhenTheBrokerHangs(t *testing.T) {
	p := newPublisher(t)
	if errs := p.Publish(t.Context(), messages(placed, 1, 1)); errs[0] != nil {
		t.Fatal(errs[0])
	}
	rabbitmqtest.Pause(t)
	returnsWithin(t, 3*time.Second, p.Close)
}

// A context that ends before the connection exists stops the dial and the
// handshake too, which no socket abort can reach yet.
func TestPublishHonoursAContextEndedWhileConnecting(t *testing.T) {
	p := newPublisher(t)
	rabbitmqtest.Pause(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var errs []error
	returnsWithin(t, time.Second, func() { errs = p.Publish(ctx, messages(placed, 1, 1)) })
	if errs[0] == nil {
		t.Error("published with an ended context")
	}
}

// A hung broker accepts bytes until the socket buffers fill and never
// confirms; Publish still returns at its deadline, including from a send
// blocked on a full socket.
func TestPublishGivesUpOnAHungBroker(t *testing.T) {
	p := newPublisher(t)
	if errs := p.Publish(t.Context(), messages(placed, 1, 1)); errs[0] != nil {
		t.Fatal(errs[0])
	}
	rabbitmqtest.Pause(t)
	var errs []error
	returnsWithin(t, 5*time.Second, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		errs = p.Publish(ctx, messages(placed, rabbitmq.MaxBatch, 128<<10)) // 64 MiB, more than the socket buffers
	})
	for i, err := range errs {
		if err == nil {
			t.Fatalf("message %d accepted by a paused broker", i)
		}
	}
}

// The policies in definitions.json do what D49, D64 and D70 require.
func TestDefinitionsPolicies(t *testing.T) {
	out := rabbitmqtest.Exec(t, "rabbitmqctl", "-q", "list_queues", "-p", rabbitmqtest.Vhost, "name", "effective_policy_definition", "--formatter", "json")
	var queues []struct {
		Name       string         `json:"name"`
		Definition map[string]any `json:"effective_policy_definition"`
	}
	if err := json.Unmarshal([]byte(out), &queues); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	const week = 7 * 24 * 3600 * 1000.0
	want := map[string]map[string]any{
		queue: {"dead-letter-exchange": "labflow.dead-letter", "dead-letter-routing-key": queue, "dead-letter-strategy": "at-least-once",
			"overflow": "reject-publish", "max-length": 100000.0, "delivery-limit": 5.0, "message-ttl": week},
		deadLetter: {"overflow": "reject-publish", "max-length": 100000.0, "message-ttl": week},
	}
	for _, q := range queues {
		if !reflect.DeepEqual(q.Definition, want[q.Name]) {
			t.Errorf("%s policy = %v, want %v", q.Name, q.Definition, want[q.Name])
		}
		delete(want, q.Name)
	}
	if len(want) > 0 {
		t.Errorf("queues missing: %v", want)
	}
}

// A message its consumer keeps returning reaches the dead-letter queue once
// the delivery limit is spent, with its ID and the reason.
func TestDeliveryLimitDeadLettersTheMessage(t *testing.T) {
	p := newPublisher(t)
	if errs := p.Publish(t.Context(), messages(placed, 1, 1)); errs[0] != nil {
		t.Fatal(errs[0])
	}
	admin := rabbitmqtest.Admin(t)
	returned := 0
	for ; returned <= 10; returned++ {
		d, ok, err := admin.Get(queue, false)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if err := d.Reject(true); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // let the queue apply the return
	}
	d := rabbitmqtest.Get(t, deadLetter)
	if returned != 6 || d.MessageId != "m0" || d.Headers["x-first-death-reason"] != "delivery_limit" {
		t.Errorf("after %d returns: %s, %v", returned, d.MessageId, d.Headers["x-first-death-reason"])
	}
}

// The producer may only publish: it cannot change the topology or read a
// queue, so it can neither silence nor receive another service's events (D70).
func TestProducerCannotChangeTheTopologyOrRead(t *testing.T) {
	conn, err := amqp.Dial(rabbitmqtest.URL(t, "order-api"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for name, op := range map[string]func(ch *amqp.Channel) error{
		"declare a queue": func(ch *amqp.Channel) error {
			_, err := ch.QueueDeclare("stolen", true, false, false, false, amqp.Table{"x-queue-type": "quorum"})
			return err
		},
		"bind a queue":   func(ch *amqp.Channel) error { return ch.QueueBind(queue, "labflow.#", rabbitmq.Exchange, false, nil) },
		"unbind a queue": func(ch *amqp.Channel) error { return ch.QueueUnbind(queue, placed, rabbitmq.Exchange, nil) },
		"delete a queue": func(ch *amqp.Channel) error { _, err := ch.QueueDelete(queue, false, false, false); return err },
		"purge a queue":  func(ch *amqp.Channel) error { _, err := ch.QueuePurge(queue, false); return err },
		"read a queue":   func(ch *amqp.Channel) error { _, _, err := ch.Get(queue, false); return err },
		"declare an exchange": func(ch *amqp.Channel) error {
			return ch.ExchangeDeclare("stolen", "fanout", true, false, false, false, nil)
		},
		"delete the exchange": func(ch *amqp.Channel) error { return ch.ExchangeDelete(rabbitmq.Exchange, false, false) },
	} {
		t.Run(name, func(t *testing.T) {
			ch, err := conn.Channel()
			if err != nil {
				t.Fatal(err)
			}
			if err := op(ch); err == nil || !strings.Contains(err.Error(), "ACCESS_REFUSED") {
				t.Errorf("error = %v, want ACCESS_REFUSED", err)
			}
		})
	}
}

func TestURLFromEnv(t *testing.T) {
	const url = "amqp://order-api:s3cret@rabbitmq:5672/labflow"
	for _, tc := range []struct {
		name, broker, url, wantErr string
	}{
		{name: "valid", broker: "rabbitmq", url: url},
		{name: "broker missing", url: url, wantErr: "BROKER"},
		{name: "another broker", broker: "kafka", url: url, wantErr: `BROKER must be rabbitmq, got "kafka"`},
		{name: "url missing", broker: "rabbitmq", wantErr: "RABBITMQ_URL"},
		{name: "not an amqp url", broker: "rabbitmq", url: "http://order-api:s3cret@rabbitmq", wantErr: "RABBITMQ_URL must be"},
		{name: "port out of range", broker: "rabbitmq", url: "amqp://order-api:s3cret@rabbitmq:70000/labflow", wantErr: "RABBITMQ_URL must be"},
		{name: "no host", broker: "rabbitmq", url: "amqp:rabbitmq", wantErr: "RABBITMQ_URL must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BROKER", tc.broker)
			t.Setenv("RABBITMQ_URL", tc.url)
			env := config.FromEnviron()
			got := rabbitmq.URLFromEnv(env)
			err := env.Err()
			if tc.wantErr == "" {
				if err != nil || got != url {
					t.Errorf("URLFromEnv = %q, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error repeats the password: %v", err)
			}
		})
	}
}

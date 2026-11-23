package order

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
)

const (
	eventSource = "/order-api"
	// TypeOrderPlaced is the CloudEvents type of a paid order (D47, D50).
	TypeOrderPlaced = "labflow.order.placed.v1"
	// outboxTopic is order-api's topic, the outbox aggregatetype (D48, D62).
	outboxTopic = "orders"
)

// event is a CloudEvents 1.0 envelope in structured JSON mode (D47).
type event struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Subject         string `json:"subject"`
	Time            string `json:"time"`
	DataContentType string `json:"datacontenttype"`
	TraceParent     string `json:"traceparent"`
	Data            any    `json:"data"`
}

type placedCustomer struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
}

type orderPlaced struct {
	OrderID  string         `json:"order_id"`
	Customer placedCustomer `json:"customer"`
	Priced
}

// placedEvent builds the order.placed event of a paid order. It carries only
// the personal data email-worker needs (D64): never the address or last name.
func placedEvent(ctx context.Context, o Order) (id string, payload []byte, err error) {
	// The W3C trace context of the change (D61); every request runs in a span.
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	if carrier["traceparent"] == "" {
		return "", nil, errors.New("order.placed needs a traceparent: the checkout ran outside a span")
	}
	e := event{
		SpecVersion: "1.0", ID: uuid.Must(uuid.NewV7()).String(), Source: eventSource, Type: TypeOrderPlaced,
		Subject: o.ID, Time: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), DataContentType: "application/json",
		TraceParent: carrier["traceparent"],
		Data: orderPlaced{OrderID: o.ID, Priced: o.Priced,
			Customer: placedCustomer{Email: o.Customer.Email, FirstName: o.Customer.FirstName}},
	}
	payload, _ = json.Marshal(e) // cannot fail: plain data
	return e.ID, payload, nil
}

package push

import (
	"crypto/tls"
	"sync"
	"time"

	"github.com/aporeto-inc/elemental"
	"github.com/aporeto-inc/manipulate"
	"github.com/aporeto-inc/manipulate/internal/wsutils"
	"github.com/gorilla/websocket"
)

const (
	eventChSize  = 1024
	errorChSize  = 64
	statusChSize = 8
)

type subscription struct {
	events            chan *elemental.Event
	errors            chan error
	status            chan manipulate.SubscriberStatus
	conn              *websocket.Conn
	stoppedLock       *sync.Mutex
	stopped           bool
	endpoint          string
	tlsConfig         *tls.Config
	maxConnRetry      int
	disconnectionFunc func()
}

// NewSubscriber creates a new Subscription.
func NewSubscriber(
	url string,
	ns string,
	token string,
	tlsConfig *tls.Config,
	recursive bool,
	maxConnRetry int,
) (manipulate.Subscriber, error) {

	s := &subscription{
		endpoint:     wsutils.MakeURL(url, "events", ns, token, recursive),
		maxConnRetry: maxConnRetry,
		tlsConfig:    tlsConfig,
		stoppedLock:  &sync.Mutex{},
		events:       make(chan *elemental.Event, eventChSize),
		errors:       make(chan error, errorChSize),
		status:       make(chan manipulate.SubscriberStatus, statusChSize),
	}

	go s.listen()

	return s, nil
}

// Unsubscribe stop the subscription. After this, the Subscription must not be reused.
func (s *subscription) Unsubscribe() error {

	s.stoppedLock.Lock()
	s.stopped = true
	s.stoppedLock.Unlock()

	if s.conn == nil {
		return nil
	}

	return s.conn.Close()
}

// UpdateFilter updates the desired filter.
func (s *subscription) UpdateFilter(filter *elemental.PushFilter) error {
	return s.conn.WriteJSON(filter)
}

// Events returns the event channel.
func (s *subscription) Events() chan *elemental.Event {
	return s.events
}

// Errors returns the error channel.
func (s *subscription) Errors() chan error {
	return s.errors
}

// Status returns the status channel.
func (s *subscription) Status() chan manipulate.SubscriberStatus {
	return s.status
}

func (s *subscription) connect() (err error) {

	try := 0

	for {
		s.conn, err = wsutils.Dial(
			s.endpoint,
			s.tlsConfig,
		)

		if err == nil || s.isStopped() {
			return nil
		}

		try++
		if s.maxConnRetry != -1 && try >= s.maxConnRetry {
			return err
		}

		time.Sleep(3 * time.Second)
	}
}

func (s *subscription) listen() {

	var isReconnection bool
	var isDisconnection bool

	// Connection loop.
	for {

		// If the subscriber is stopped, we return.
		if s.isStopped() {
			return
		}

		// If this is a disconnection, we publish the status event.
		if isDisconnection {
			select {
			case s.status <- manipulate.SubscriberStatusDisconnection:
			default:
			}
		}

		// If we can't connect we publish the error and we return.
		// note: this will return the error only if it could not connect
		// after the configured number of failed tries.
		if err := s.connect(); err != nil {
			s.errors <- err
			return
		}

		// If this is a reconnection we publish the reconnection event
		// otherwise we publish the initial connection event.
		event := manipulate.SubscriberStatusInitialConnection
		if isReconnection {
			event = manipulate.SubscriberStatusReconnection
		}
		select {
		case s.status <- event:
		default:
		}

		isReconnection = true
		isDisconnection = true

		// Read loop.
		for {

			event := &elemental.Event{}
			err := s.conn.ReadJSON(event)

			// If there is no error, we publish the event and  we continue the read loop.
			if err == nil {
				select {
				case s.events <- event:
				default:
				}
				continue
			}

			// If the error is an abrupt connection close (server is gone)
			// we break the read loop, and continue the connection loop.
			if websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
				break
			}

			// Otherwise it's a protocol error, we an publish the err
			// and we continue the read loop.
			select {
			case s.errors <- err:
			default:
			}
		}
	}
}

func (s *subscription) isStopped() bool {

	s.stoppedLock.Lock()
	stopped := s.stopped
	s.stoppedLock.Unlock()

	return stopped
}

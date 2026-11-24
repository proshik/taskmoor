package serve

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Push notifications to paired devices (stage 10): a device's board,
// installed as an app, subscribes with the browser's push service; when an
// ask comes to the person (as Telegram is told) and when it is reminded
// (A20), each subscription gets an encrypted message naming it. An
// approval's message carries nothing of the request, as in Telegram.

// PushSub is a device's subscription.
type PushSub struct {
	Device   string `json:"device"`
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
	Created  string `json:"created"`
}

type pushState struct {
	mu       sync.Mutex
	loaded   bool
	key      *vapidKey
	subs     []PushSub
	pushed   map[string]bool      // asks whose first message is queued or sent
	reminded map[string]time.Time // the last reminder event of an ask that was queued
	version  map[string]int       // an ask's latest message: older retries are dropped
	behind   bool                 // the queue overflowed: reconcile once it drains
	pending  []pushJob            // the queue, at most limit jobs
	limit    int
	wake     chan struct{}
	// gate orders sending and subscribing (read) against a device's
	// revocation (write): once RevokeDevice returns, no new POST or
	// subscription reaches it (a message a push service has already
	// accepted may still arrive, until its TTL)
	gate      sync.RWMutex
	wg        sync.WaitGroup
	writeFail func() error // tests: push.json cannot be written
}

const maxPushQueue = 1024

func newPushState() pushState {
	return pushState{pushed: map[string]bool{}, reminded: map[string]time.Time{}, version: map[string]int{},
		wake: make(chan struct{}, 1), limit: maxPushQueue}
}

func (s *Service) pushPath() string  { return filepath.Join(s.Paths.Store, "push.json") }
func (s *Service) vapidPath() string { return filepath.Join(s.Paths.Store, "vapid.json") }

// pushLoad reads the key (made on first use) and the subscriptions;
// push.mu is held.
func (s *Service) pushLoad() error {
	if s.push.loaded {
		return nil
	}
	var k vapidKey
	data, err := os.ReadFile(s.vapidPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		if k, err = newVapidKey(); err != nil {
			return err
		}
		if err := writeJSONFile(s.vapidPath(), k); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &k); err != nil {
			return fmt.Errorf("%s: %w", s.vapidPath(), err)
		}
	}
	if _, err := k.ecdsa(); err != nil {
		return fmt.Errorf("%s: %w", s.vapidPath(), err)
	}
	var subs []PushSub
	if data, err := os.ReadFile(s.pushPath()); err == nil {
		if err := json.Unmarshal(data, &subs); err != nil {
			return fmt.Errorf("%s: %w", s.pushPath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.push.key, s.push.subs, s.push.loaded = &k, subs, true
	return nil
}

// pushWrite keeps subs; push.mu is held, and nothing changes in memory
// unless it was written.
func (s *Service) pushWrite(subs []PushSub) error {
	if subs == nil {
		subs = []PushSub{}
	}
	if f := s.push.writeFail; f != nil {
		if err := f(); err != nil {
			return err
		}
	}
	if err := writeJSONFile(s.pushPath(), subs); err != nil {
		return err
	}
	s.push.subs = subs
	return nil
}

// PushKey is the application server key devices subscribe with.
func (s *Service) PushKey() (string, error) {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if err := s.pushLoad(); err != nil {
		return "", err
	}
	return s.push.key.publicKey()
}

// PushSubscribe keeps a device's subscription (one per endpoint).
func (s *Service) PushSubscribe(device string, sub PushSub) error {
	if !pushHostOK(sub.Endpoint) {
		return refusedf("that push endpoint is not a known push service's")
	}
	p, err := b64url.DecodeString(sub.P256dh)
	if err != nil {
		return refusedf("the subscription's p256dh key is not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(p); err != nil { // on the curve, not just the right length
		return refusedf("the subscription's p256dh key is not a P-256 point")
	}
	if a, err := b64url.DecodeString(sub.Auth); err != nil || len(a) != 16 {
		return refusedf("the subscription's auth secret is not 16 bytes")
	}
	// the device must still be paired as the subscription is written: a
	// revocation is either before (refused) or after (it removes this)
	s.push.gate.RLock()
	defer s.push.gate.RUnlock()
	if !s.deviceExists(device) {
		return refusedf("this device is no longer paired")
	}
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if err := s.pushLoad(); err != nil {
		return err
	}
	var next []PushSub
	mine := 0
	for _, x := range s.push.subs {
		if x.Endpoint == sub.Endpoint {
			continue // replaced
		}
		if x.Device == device {
			mine++
		}
		next = append(next, x)
	}
	if mine >= 4 {
		return refusedf("this device has 4 subscriptions already: turn one off first")
	}
	sub.Device, sub.Created = device, stamp(s.Now())
	return s.pushWrite(append(next, sub))
}

// PushUnsubscribe forgets a device's subscription (all of the device's
// when endpoint is "").
func (s *Service) PushUnsubscribe(device, endpoint string) error {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if err := s.pushLoad(); err != nil {
		return err
	}
	var next []PushSub
	for _, x := range s.push.subs {
		if x.Device == device && (endpoint == "" || x.Endpoint == endpoint) {
			continue
		}
		next = append(next, x)
	}
	return s.pushWrite(next)
}

// PushSubscribed says whether a device has a subscription for endpoint.
func (s *Service) pushSubscribed(device, endpoint string) bool {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if s.pushLoad() != nil {
		return false
	}
	for _, x := range s.push.subs {
		if x.Device == device && x.Endpoint == endpoint {
			return true
		}
	}
	return false
}

// pushClient follows no redirect: an endpoint is the push service's own.
var pushClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// pushMessage is what the board's service worker shows.
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`
	URL   string `json:"url"`
}

// askPush is an ask's notification: nothing of an approval's request.
func askPush(a *Ask, reminder bool) pushMessage {
	m := pushMessage{Title: a.Project + " · " + a.Task, Tag: "ask-" + a.ID, URL: "/#inbox"}
	switch {
	case a.Kind != "question":
		m.Body = "An approval waits for you"
	case len(a.Questions) > 0 && !a.Questions[0].Secret:
		m.Body = clip(a.Questions[0].Question, 180)
	default:
		m.Body = "A question waits for you"
	}
	if reminder {
		m.Body = "Still waiting: " + m.Body
	}
	return m
}

// pushJob is a message to send: to every subscription, or (a retry) to
// one. ask and version tie it to its ask's latest message.
type pushJob struct {
	msg      pushMessage
	ask      string
	version  int
	endpoint string
	attempt  int
}

const maxPushAttempts = 4

// enqueue queues a job without waiting; false when the queue is full (the
// sender reconciles the open asks once it drains).
func (s *Service) enqueue(j pushJob) bool {
	s.push.mu.Lock()
	ok := len(s.push.pending) < s.push.limit
	if ok {
		s.push.pending = append(s.push.pending, j)
	} else {
		s.push.behind = true
	}
	s.push.mu.Unlock()
	if ok {
		select {
		case s.push.wake <- struct{}{}:
		default:
		}
	}
	return ok
}

// next takes the oldest job (false: none).
func (s *Service) next() (pushJob, bool) {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if len(s.push.pending) == 0 {
		return pushJob{}, false
	}
	j := s.push.pending[0]
	s.push.pending = s.push.pending[1:]
	return j, true
}

// current says whether a job is still worth a POST: its ask open and for
// the person, and no newer message of it queued since.
func (s *Service) current(j pushJob) bool {
	s.mu.Lock()
	a := s.asks[j.ask]
	open := a != nil && a.opened && forPerson(a)
	s.mu.Unlock()
	s.push.mu.Lock()
	latest := s.push.version[j.ask] == j.version
	s.push.mu.Unlock()
	return open && latest
}

// deliver sends a job. Each POST is admitted under the gate revocation
// takes, with the device checked as paired.
func (s *Service) deliver(ctx context.Context, j pushJob) {
	s.push.mu.Lock()
	if s.pushLoad() != nil {
		s.push.mu.Unlock()
		return
	}
	subs := append([]PushSub{}, s.push.subs...)
	key := *s.push.key
	s.push.mu.Unlock()
	subject := "mailto:taskmoor@localhost"
	if cfg, err := s.Config(); err == nil && cfg.RemoteHost != "" {
		subject = "https://" + cfg.RemoteHost
	}
	payload, _ := json.Marshal(j.msg)
	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}
		if (j.endpoint != "" && sub.Endpoint != j.endpoint) || !pushHostOK(sub.Endpoint) {
			continue
		}
		if !s.current(j) {
			return // answered, withdrawn, or a newer message of it is on its way
		}
		s.push.gate.RLock()
		if exists, known := s.deviceState(sub.Device); !exists {
			s.push.gate.RUnlock()
			if known {
				_ = s.PushUnsubscribe(sub.Device, sub.Endpoint) // a revoked device's leftover
			}
			continue
		}
		status, wait, err := s.postPush(ctx, sub, key, subject, payload, j.msg.Tag)
		s.push.gate.RUnlock()
		switch {
		case err == nil && (status == http.StatusNotFound || status == http.StatusGone):
			_ = s.PushUnsubscribe(sub.Device, sub.Endpoint) // the browser let it go
		case err != nil || status == http.StatusTooManyRequests || status >= 500:
			// refused for now: that subscription again, later (RFC 8030 §8.4)
			if j.attempt+1 < maxPushAttempts {
				retry := j
				retry.endpoint, retry.attempt = sub.Endpoint, j.attempt+1
				time.AfterFunc(wait, func() {
					if ctx.Err() == nil && s.current(retry) {
						s.enqueue(retry)
					}
				})
			}
		}
	}
}

// postPush sends one encrypted message; wait is when to try again.
func (s *Service) postPush(ctx context.Context, sub PushSub, key vapidKey, subject string, payload []byte, tag string) (int, time.Duration, error) {
	wait := time.Minute
	p, _ := b64url.DecodeString(sub.P256dh)
	a, _ := b64url.DecodeString(sub.Auth)
	body, err := encryptPush(payload, p, a, nil, nil)
	if err != nil {
		return 0, wait, err
	}
	auth, err := vapidAuth(key, sub.Endpoint, subject, s.Now())
	if err != nil {
		return 0, wait, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, wait, err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(pushTTL/time.Second)))
	req.Header.Set("Urgency", "high")
	topic := strings.ReplaceAll(tag, "-", "") // one per ask: a newer one replaces it
	if len(topic) > 32 {
		topic = topic[:32]
	}
	req.Header.Set("Topic", topic)
	req.Header.Set("Authorization", auth)
	resp, err := pushClient.Do(req)
	if err != nil {
		return 0, wait, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
			wait = time.Duration(n) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			wait = time.Until(t)
		}
	}
	wait = min(max(wait, time.Second), 10*time.Minute)
	return resp.StatusCode, wait, nil
}

// pushTTL is how long a push service keeps a message for an offline device.
const pushTTL = 6 * time.Hour

// startPush follows the asks until ctx ends; stopPush waits for it. The
// follower only queues: the sender posts, so a slow push service never
// holds the event stream.
func (s *Service) startPush(ctx context.Context) {
	s.push.wg.Add(2)
	go func() {
		defer s.push.wg.Done()
		gc := time.NewTicker(time.Minute)
		defer gc.Stop()
		s.pushGC()
		for {
			select {
			case <-ctx.Done():
				return
			case <-gc.C:
				s.pushGC() // revoked devices' leftovers
				continue
			case <-s.push.wake:
			}
			for {
				j, ok := s.next()
				if !ok || ctx.Err() != nil {
					break
				}
				s.deliver(ctx, j)
			}
			s.push.mu.Lock()
			behind := s.push.behind
			s.push.behind = false
			s.push.mu.Unlock()
			if behind {
				s.pushReconcile()
			}
		}
	}()
	go func() {
		defer s.push.wg.Done()
		for ctx.Err() == nil {
			events, unsub := s.bus.subscribe()
			s.pushReconcile() // what came while no one listened
		recv:
			for {
				select {
				case <-ctx.Done():
					break recv
				case ev, ok := <-events:
					if !ok {
						break recv
					}
					s.pushEvent(ev)
				}
			}
			unsub()
		}
	}()
}

func (s *Service) stopPush() { s.push.wg.Wait() }

// pushGC forgets the subscriptions of devices no longer paired (a
// revocation whose cleanup could not be written).
func (s *Service) pushGC() {
	s.push.mu.Lock()
	if s.pushLoad() != nil {
		s.push.mu.Unlock()
		return
	}
	subs := append([]PushSub{}, s.push.subs...)
	s.push.mu.Unlock()
	for _, sub := range subs {
		if exists, known := s.deviceState(sub.Device); known && !exists {
			_ = s.PushUnsubscribe(sub.Device, sub.Endpoint)
		}
	}
}

// pushReconcile queues what the open asks are owed: a first message never
// queued, a reminder whose event was missed.
func (s *Service) pushReconcile() {
	s.mu.Lock()
	var open []Ask
	for _, id := range s.order {
		if a := s.asks[id]; a != nil && a.opened && forPerson(a) {
			open = append(open, *a)
		}
	}
	s.mu.Unlock()
	for i := range open {
		a := &open[i]
		s.pushEvent(Event{Type: "ask", Data: *a})
		// the clients' reminder clock (telegram.go): a reminder published
		// after the last one queued here was missed
		s.tg.mu.Lock()
		evt, had := s.tg.evtAt[a.ID]
		s.tg.mu.Unlock()
		created, _ := time.Parse(time.RFC3339, a.CreatedAt)
		s.push.mu.Lock()
		last := s.push.reminded[a.ID]
		s.push.mu.Unlock()
		if had && evt.After(created) && evt.After(last) {
			s.queueAsk(a, true, evt)
		}
	}
}

// queueAsk queues an ask's message as its latest version.
func (s *Service) queueAsk(a *Ask, reminder bool, at time.Time) bool {
	s.push.mu.Lock()
	s.push.version[a.ID]++
	v := s.push.version[a.ID]
	s.push.mu.Unlock()
	ok := s.enqueue(pushJob{msg: askPush(a, reminder), ask: a.ID, version: v})
	if ok {
		s.push.mu.Lock()
		if reminder {
			s.push.reminded[a.ID] = at
		} else {
			s.push.pushed[a.ID] = true
		}
		s.push.mu.Unlock()
	}
	return ok
}

func (s *Service) pushEvent(ev Event) {
	switch ev.Type {
	case "ask":
		a, ok := ev.Data.(Ask)
		if !ok || !forPerson(&a) {
			return
		}
		s.push.mu.Lock()
		done := s.push.pushed[a.ID]
		s.push.mu.Unlock()
		if !done {
			s.queueAsk(&a, false, time.Time{}) // marked only once queued
		}
	case "ask_reminder":
		m, ok := ev.Data.(map[string]string)
		if !ok {
			return
		}
		s.mu.Lock()
		a := s.asks[m["id"]]
		var cp Ask
		if a != nil {
			cp = *a
		}
		s.mu.Unlock()
		if a != nil && forPerson(&cp) {
			s.queueAsk(&cp, true, s.Now())
		}
	case "ask_resolved":
		if m, ok := ev.Data.(map[string]string); ok {
			s.push.mu.Lock()
			delete(s.push.pushed, m["id"])
			delete(s.push.reminded, m["id"])
			delete(s.push.version, m["id"])
			s.push.mu.Unlock()
		}
	}
}

package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/sessions"
)

// memoStore wraps a *sessions.CookieStore and memoises the result of a *successful* cookie decode
// (base64 + HMAC-SHA256 verify + gob decode of the Values map; ~9% of the app's CPU in the pprof
// profile because verifyUserSession / session.Get runs on almost every request) keyed by the exact
// cookie string. Only cookies that the underlying store already verified are ever cached, so an
// invalid or tampered cookie always takes the normal (failing) path; a hit returns a fresh Session
// with a shallow copy of the values (all int64/string, and loginHandler mutates Values), so callers
// can never alias the cached map. Entries expire after memoTTL (well inside the 30-day securecookie
// max-age; the app-level EXPIRES check in verifyUserSession still runs on every request) and the
// whole map is dropped if it ever grows beyond memoMax. Save is delegated unchanged.
type memoStore struct {
	inner *sessions.CookieStore
	cache sync.Map // cookie value -> memoEntry
	n     int64
}

type memoEntry struct {
	values  map[interface{}]interface{}
	expires time.Time
}

const (
	memoTTL = 10 * time.Minute
	memoMax = 200000
)

func newMemoStore(inner *sessions.CookieStore) *memoStore {
	return &memoStore{inner: inner}
}

func (s *memoStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(s, name)
}

func (s *memoStore) New(r *http.Request, name string) (*sessions.Session, error) {
	c, errCookie := r.Cookie(name)
	if errCookie == nil {
		if v, ok := s.cache.Load(c.Value); ok {
			e := v.(memoEntry)
			if time.Now().Before(e.expires) {
				sess := sessions.NewSession(s, name)
				opts := *s.inner.Options
				sess.Options = &opts
				sess.IsNew = false
				for k, val := range e.values {
					sess.Values[k] = val
				}
				return sess, nil
			}
			s.cache.Delete(c.Value)
		}
	}

	sess, err := s.inner.New(r, name)
	if err == nil && errCookie == nil && !sess.IsNew {
		values := make(map[interface{}]interface{}, len(sess.Values))
		for k, val := range sess.Values {
			values[k] = val
		}
		if atomic.AddInt64(&s.n, 1) > memoMax {
			s.cache.Range(func(k, _ any) bool { s.cache.Delete(k); return true })
			atomic.StoreInt64(&s.n, 0)
		}
		s.cache.Store(c.Value, memoEntry{values: values, expires: time.Now().Add(memoTTL)})
	}
	return sess, err
}

func (s *memoStore) Save(r *http.Request, w http.ResponseWriter, sess *sessions.Session) error {
	return s.inner.Save(r, w, sess)
}
